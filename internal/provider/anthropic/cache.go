package anthropic

import (
	"bytes"
	"encoding/json"
)

// CacheStrategy selects how aggressively cerber injects Anthropic prompt-cache
// breakpoints (`cache_control`) into a native /v1/messages request. Anthropic
// caches request prefixes in the order tools -> system -> messages; a breakpoint
// on a block caches everything up to and including it. More breakpoints let a
// growing conversation reuse progressively longer prefixes (max 4 per request).
type CacheStrategy string

const (
	// CacheConservative caches only the stable tool + system prefix (<=2 markers).
	CacheConservative CacheStrategy = "conservative"
	// CacheModerate adds one message-history breakpoint (<=3 markers). Default.
	CacheModerate CacheStrategy = "moderate"
	// CacheAggressive adds a second message-history breakpoint (<=4 markers).
	CacheAggressive CacheStrategy = "aggressive"
)

// maxBreakpoints is Anthropic's hard limit on cache_control markers per request.
const maxBreakpoints = 4

// ephemeralCacheControl is the marker cerber injects. Plain `ephemeral` uses the
// default 5-minute TTL and needs no beta header, so it is universally safe. The
// 1-hour TTL variant requires `anthropic-beta: extended-cache-ttl-*` and is left
// as a future opt-in.
var ephemeralCacheControl = json.RawMessage(`{"type":"ephemeral"}`)

// CacheOptions configures automatic cache-breakpoint injection. The zero value
// is disabled, so the native path stays a pure passthrough unless opted in.
type CacheOptions struct {
	Enabled  bool
	Strategy CacheStrategy
	// MinTokens is the estimated-token floor a cached prefix must reach to earn a
	// breakpoint. A marker below Anthropic's minimum cacheable length is ignored
	// upstream and would only waste one of the four slots.
	MinTokens int
}

// budget returns how many cache_control markers this strategy may place.
func (s CacheStrategy) budget() int {
	switch s {
	case CacheConservative:
		return 2
	case CacheAggressive:
		return maxBreakpoints
	default: // moderate / empty / unknown
		return 3
	}
}

// InjectCacheBreakpoints rewrites a native Anthropic request body to add
// `cache_control: {"type":"ephemeral"}` markers on the largest stable prefixes,
// following Anthropic's prefix-cache order (tools, then system, then message
// history). It is deliberately conservative and non-destructive:
//
//   - disabled (opt.Enabled == false) -> body returned unchanged.
//   - if the caller already set ANY cache_control, cerber does not touch the
//     request: the client owns its own breakpoints and the 4-marker budget.
//   - a prefix earns a breakpoint only when its estimated size reaches
//     MinTokens (a smaller marker is ignored upstream, wasting a slot).
//   - message-history breakpoints are placed only on content already in block
//     form; string content is never reshaped into blocks.
//   - on any parse/marshal error the ORIGINAL body is returned alongside the
//     error, so the caller can forward the request unmodified rather than fail
//     it. Injection must never break a request.
//
// The returned bool reports whether the body was actually changed.
func InjectCacheBreakpoints(body []byte, opt CacheOptions) ([]byte, bool, error) {
	if !opt.Enabled {
		return body, false, nil
	}
	// Respect a client that already manages its own cache breakpoints. This is a
	// cheap substring check; a false match (the literal appearing in user text)
	// only causes cerber to skip injection, which is the safe direction.
	if bytes.Contains(body, []byte("cache_control")) {
		return body, false, nil
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body, false, err
	}

	budget := opt.Strategy.budget()
	placed := 0

	toolsEst := estTokens(m["tools"])
	sysEst := estTokens(m["system"])

	// 1) system breakpoint: caches the tools+system prefix, the highest-value and
	// most stable prefix in agent workloads. Placed first.
	if placed < budget && toolsEst+sysEst >= opt.MinTokens {
		if raw, ok := m["system"]; ok {
			if newRaw, ok := markSystem(raw); ok {
				m["system"] = newRaw
				placed++
			}
		}
	}

	// 2) tools breakpoint: an incremental, tools-only prefix that survives even
	// when the system text changes between requests.
	if placed < budget && toolsEst >= opt.MinTokens {
		if raw, ok := m["tools"]; ok {
			if newRaw, ok := markLastArrayElem(raw); ok {
				m["tools"] = newRaw
				placed++
			}
		}
	}

	// 3/4) message-history breakpoints (moderate: 1, aggressive: up to 2). Skipped
	// for conservative. Gated on the full cumulative prefix size.
	if opt.Strategy != CacheConservative && placed < budget {
		want := budget - placed
		if opt.Strategy == CacheModerate && want > 1 {
			want = 1
		}
		if raw, ok := m["messages"]; ok && toolsEst+sysEst+estTokens(raw) >= opt.MinTokens {
			if newRaw, n := markMessagesLastBlocks(raw, want); n > 0 {
				m["messages"] = newRaw
				placed += n
			}
		}
	}

	if placed == 0 {
		return body, false, nil
	}
	out, err := json.Marshal(m)
	if err != nil {
		return body, false, err
	}
	return out, true, nil
}

// estTokens approximates the token count of a raw JSON region as bytes/4. It
// intentionally overcounts (JSON punctuation is included), biasing toward
// caching; a prefix that turns out below Anthropic's real minimum is simply
// ignored upstream. null/empty regions count as zero.
func estTokens(raw json.RawMessage) int {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	return len(raw) / 4
}

// markSystem adds a cache_control marker to the Anthropic `system` value. A
// string is converted to a single cached text block (the same shape spoof.go
// already produces); an array has its last block marked in place, preserving any
// unknown fields. Returns false if there is nothing markable.
func markSystem(raw json.RawMessage) (json.RawMessage, bool) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return nil, false
		}
		block := map[string]json.RawMessage{
			"type":          json.RawMessage(`"text"`),
			"text":          raw, // raw is already the JSON-quoted string
			"cache_control": ephemeralCacheControl,
		}
		out, err := json.Marshal([]map[string]json.RawMessage{block})
		if err != nil {
			return nil, false
		}
		return out, true
	}
	return markLastArrayElem(raw)
}

// markLastArrayElem marks the final element of a JSON array (e.g. tools) with a
// cache_control field, preserving all other fields of that element.
func markLastArrayElem(raw json.RawMessage) (json.RawMessage, bool) {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil || len(arr) == 0 {
		return nil, false
	}
	marked, ok := addCacheControl(arr[len(arr)-1])
	if !ok {
		return nil, false
	}
	arr[len(arr)-1] = marked
	out, err := json.Marshal(arr)
	if err != nil {
		return nil, false
	}
	return out, true
}

// markMessagesLastBlocks marks the last content block of up to n messages,
// walking from the newest, skipping messages whose content is not a block array
// (string content is never reshaped). Returns the rewritten array and the number
// of breakpoints actually placed.
func markMessagesLastBlocks(raw json.RawMessage, n int) (json.RawMessage, int) {
	if n <= 0 {
		return nil, 0
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(raw, &msgs); err != nil || len(msgs) == 0 {
		return nil, 0
	}
	placed := 0
	for i := len(msgs) - 1; i >= 0 && placed < n; i-- {
		newMsg, ok := markMessageContentLastBlock(msgs[i])
		if !ok {
			continue
		}
		msgs[i] = newMsg
		placed++
	}
	if placed == 0 {
		return nil, 0
	}
	out, err := json.Marshal(msgs)
	if err != nil {
		return nil, 0
	}
	return out, placed
}

// markMessageContentLastBlock marks the final block of one message's content,
// but only when that content is already a JSON array of blocks. String content
// returns false (left untouched) to avoid changing the request shape.
func markMessageContentLastBlock(msg json.RawMessage) (json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(msg, &obj); err != nil {
		return nil, false
	}
	content, ok := obj["content"]
	if !ok {
		return nil, false
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(content, &blocks); err != nil || len(blocks) == 0 {
		return nil, false // string content, or empty — do not reshape
	}
	marked, ok := addCacheControl(blocks[len(blocks)-1])
	if !ok {
		return nil, false
	}
	blocks[len(blocks)-1] = marked
	nc, err := json.Marshal(blocks)
	if err != nil {
		return nil, false
	}
	obj["content"] = nc
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return out, true
}

// addCacheControl sets cache_control on a JSON object element, preserving its
// other fields. Returns false if the element is not a JSON object.
func addCacheControl(elem json.RawMessage) (json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(elem, &obj); err != nil {
		return nil, false
	}
	obj["cache_control"] = ephemeralCacheControl
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return out, true
}
