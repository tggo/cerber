# Provider: ComfyUI (local GGUF on a shared GPU)

If one GPU already runs ComfyUI for image or video work, a second inference
server such as Ollama next to it competes for VRAM: both processes hold models
without knowing about each other, and eventually one of them runs out of memory.

The `comfyui` provider avoids that by running the LLM **inside ComfyUI**, through
the [comfyui-cerber-llm](https://github.com/tggo/comfyui-cerber-llm) custom node:

- A chat request becomes a one-node ComfyUI prompt, so it waits its turn in the
  same queue as image jobs.
- Before loading, the node asks ComfyUI's memory manager for room. After
  answering it unloads the model and gives the VRAM back.
- Models are GGUF files under ComfyUI's `models/LLM` or `models/gguf`, or
  anything pulled with Ollama (loaded straight from the blob; the Ollama daemon
  can stay stopped).

The cost is speed: every request loads the model. Expect seconds to a minute
before the first byte. It suits a few prompts now and then, not bulk traffic.

## Setup

1. Install the node in ComfyUI (see its README): clone it into
   `ComfyUI/custom_nodes` and install `llama-cpp-python` built with CUDA. Then
   restart ComfyUI.
2. Point cerber at ComfyUI:

```yaml
providers:
  comfyui:
    base_url: "http://gpu-box:8188"
    # max_wait: 30m        # give up on a prompt that hasn't finished
    # poll_interval: 500ms # how often cerber checks /history
    # n_ctx: 8192          # context window
    # max_tokens: 1024     # output cap when the request sets none
    # keep_loaded: false   # true = leave the model in VRAM between requests
    # node: CerberLLMChat  # node class, if you renamed it
```

ComfyUI's HTTP API has no authentication, so there are no credentials. Keep
`base_url` on a trusted network: anyone who can reach ComfyUI can queue work on
it.

## Using it

cerber lists the node's models every probe cycle as `comfyui-<model>`, e.g.
`comfyui-ollama/gemma3:12b`. Any model starting with `comfyui-` routes here.

```sh
curl $CERBER/v1/chat/completions -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' \
  -d '{"model":"comfyui-ollama/gemma3:12b","messages":[{"role":"user","content":"say pong"}]}'
```

| Request field | Behaviour |
|---|---|
| `messages` | `system`/`developer`/`user`/`assistant`. Content is a string or text parts. Images, `tool` messages → 400. |
| `tools`, `n > 1` | 400 (not supported) |
| `max_tokens` / `max_completion_tokens` | output cap (default `max_tokens` from config) |
| `temperature`, `top_p`, `stop` | passed to llama.cpp |
| `seed` | passed through; random when omitted, so repeat requests sample fresh |
| `reasoning_effort` | anything but `none` enables the model's thinking mode; thinking returns as `reasoning_content` |
| `stream` | supported, but the answer arrives in one chunk once it is complete. Until then cerber sends SSE keep-alive comments so proxies don't drop the connection. |

Errors:

- **400** means ComfyUI rejected the prompt, typically a model that is no longer
  in the node's list, or the request asks for something above.
- **502** means the run failed. The message carries the node's exception, for
  example a GGUF that this llama.cpp build can't load. The node then hides that
  model from its list.
- A stream that fails after it has started ends with a `data: {"error": …}`
  event.

If the client disconnects while the prompt is still queued, cerber removes it
from ComfyUI's queue. A prompt that is already running finishes. cerber never
calls `/interrupt`, because that would stop whatever is running, which could be
someone else's image job.

Usage is recorded with token counts and zero cost.
