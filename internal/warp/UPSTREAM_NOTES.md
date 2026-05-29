# WARP Upstream Notes

These notes track facts verified against WARP's open-source client and the pinned
`warp-proto-apis` revision used by that client.

## Auth Storage

The stable Windows app stores the persisted user at:

`%LOCALAPPDATA%\warp\Warp\data\dev.warp.Warp-User`

The file is encrypted with Windows DPAPI. After decryption, the refresh token is
stored at `id_token.refresh_token`. A top-level `refresh_token` may be legacy or
empty.

## Multi-Agent Transport

The client posts protobuf requests to:

`https://app.warp.dev/ai/multi-agent`

Responses are `text/event-stream`; each SSE `data:` payload is base64-url-safe
protobuf for `warp.multi_agent.v1.ResponseEvent`.

## Protocol Source

WARP currently pins:

`https://github.com/warpdotdev/warp-proto-apis.git@c67de64fc4949f693a679552dc88cebc9f7d0180`

The public `warpdotdev/warp` repository currently points at this same proto
revision. A shallow checkout of that repository did not include the Rust
application sources that consume the multi-agent API, so the reliable comparison
surface is the pinned proto API, not a visible client implementation.

The useful files are under `apis/multi_agent/v1`:

- `request.proto`
- `response.proto`
- `task.proto`

The generated Go code in that repository is not published as an importable Go
module. Importing it directly would require vendoring/generated code or adding a
local generation step.

## Borrowed Behaviors

- Prefer nested persisted tokens such as `id_token.refresh_token`.
- Match official WARP headers for client version and OS metadata.
- Parse SSE base64 protobuf response events.
- Keep fallback tool field numbers aligned with `task.proto`.

## Current Request Shape

Decoded with the pinned `request.proto`, `realRequestTemplate` is a valid
`warp.multi_agent.v1.Request` with:

- `settings.model_config.base = "claude-4-5-opus"`
- `settings.model_config.cli_agent = "cli-agent-auto"`
- `settings.supports_parallel_tool_calls = true`
- `settings.supports_reasoning_message = true`
- `settings.web_search_enabled = true`
- `settings.supports_v4a_file_diffs = true`
- `settings.supported_tools` includes shell, file, grep, MCP, subagent,
  document, and prompt-suggestion tool types.
- `settings.supported_cli_agent_tools` includes long-running shell output,
  grep, glob, read files, and search codebase.

The request builder patches only the `model_config.base` value in this template.
It does not currently populate the newer `coding`, `computer_use_agent`,
`base_model_context_window_limit`, `custom_model_providers`,
`supports_bundled_skills`, `supports_research_agent`, or
`supports_orchestration_v2` fields.

## Known Differences

- Official `Request.Settings.ModelConfig` is role-specific. We send one patched
  base model and retain the template's `cli_agent = "cli-agent-auto"`.
- Official `MCPContext.resources` and `MCPContext.tools` are deprecated in favor
  of grouped `MCPContext.servers`. We still send top-level tools for
  compatibility.
- Official `ResponseEvent.StreamFinished.should_refresh_model_config` tells the
  client when its model config is stale. We now parse and log this signal, but
  do not yet trigger an automatic model refresh.
- Official `StreamFinished` contains request cost and conversation usage
  metadata. We currently keep token usage only.
- The public client repository's full multi-agent consumer code was not present
  in the shallow checkout, so further behavioral parity should be validated by
  generated proto round-trip tests and live debug traces.

## Recommended Next Step

Replace `realRequestTemplate` and hand-written protobuf patching with generated
protobuf types from `warp-proto-apis`. That is the largest stability win, but it
should be done as a separate change because it introduces generated source or a
new code-generation workflow.

After that, add a model-refresh service path that can consume
`should_refresh_model_config` by calling the existing Warp GraphQL model
discovery queries and updating the local model store atomically.
