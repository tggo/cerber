# Client usage

cerber accepts two request dialects. Point your client at cerber's address and use
one of your configured `access.keys` as the API key.

Assume cerber runs at `http://localhost:8080` and a client key `my-client-key`.

## OpenAI-compatible (`/v1/chat/completions`)

Works with any OpenAI-style client. Set the base URL to cerber and the API key to
a cerber access key. cerber **routes by model name**: `gpt*/o1*/o3*/o4*/chatgpt*` →
OpenAI, `gemini*` → Gemini, everything else (e.g. `claude*`) → Anthropic. Override
with `providers.routing` in the config. The target provider must be configured.

### curl

```bash
curl -s http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer my-client-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4-6",
    "messages": [{"role": "user", "content": "Say hi in one word."}]
  }'
```

### Streaming

```bash
curl -N http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer my-client-key" \
  -d '{"model":"claude-sonnet-4-6","stream":true,
       "messages":[{"role":"user","content":"count to 3"}]}'
# -> Server-Sent Events: chat.completion.chunk ... then `data: [DONE]`
```

### OpenAI Python SDK

```python
from openai import OpenAI

client = OpenAI(base_url="http://localhost:8080/v1", api_key="my-client-key")
resp = client.chat.completions.create(
    model="claude-sonnet-4-6",
    messages=[{"role": "user", "content": "hello"}],
)
print(resp.choices[0].message.content)
```

**Supported:** text and image content parts, `max_tokens` (defaults to 4096),
`temperature`, `top_p`, `stop` (string or array), streaming.
**Not yet:** `tools` / function calling on this endpoint — use the native endpoint
for tools.

## Anthropic native (`/v1/messages`)

A transparent pass-through (request and response bodies are Anthropic's own
format). Use this for full Anthropic features, including tools. cerber injects the
credential and, for OAuth, the Claude Code system prefix.

```bash
curl -s http://localhost:8080/v1/messages \
  -H "Authorization: Bearer my-client-key" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4-6",
    "max_tokens": 256,
    "messages": [{"role": "user", "content": "hello"}]
  }'
```

### Anthropic Python SDK

```python
import anthropic

client = anthropic.Anthropic(base_url="http://localhost:8080", api_key="my-client-key")
msg = client.messages.create(
    model="claude-sonnet-4-6",
    max_tokens=256,
    messages=[{"role": "user", "content": "hello"}],
)
print(msg.content[0].text)
```

## Authentication errors

| Status | Meaning |
|---|---|
| `401` | Missing or invalid client key. |
| `400` | Malformed request (e.g. OpenAI body that can't be translated). |
| `502` | Upstream/credential error (or refresh failure). |
| `503` | All upstream credentials are in cooldown. |
