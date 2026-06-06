# WARP Upstream Notes

These notes track facts verified against WARP's open-source client at
`warpdotdev/warp@1c2d4cc` and the pinned `warp-proto-apis` revision used by
that client.

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
revision and includes Rust application sources that consume the multi-agent API.
The most relevant upstream comparison points are:

- `app/src/ai/agent/api/impl.rs`
- `app/src/server/server_api.rs`
- `app/src/server/server_api/ai.rs`
- `crates/graphql/src/api/queries/get_feature_model_choices.rs`
- `crates/warp_graphql_schema/api/schema.graphql`

The useful files are under `apis/multi_agent/v1`:

- `request.proto`
- `response.proto`
- `task.proto`

The generated Go package is importable through
`github.com/warpdotdev/warp-proto-apis/apis/multi_agent/v1/gen/go`. Production
now marshals this official request type directly; the previous captured
hex/byte-template builder has been removed.

## Borrowed Behaviors

- Prefer nested persisted tokens such as `id_token.refresh_token`.
- Match official WARP headers for client version and OS metadata.
- Suppress `User-Agent` and rely on `X-Warp-Client-ID`, matching Warp's custom
  client-role header behavior.
- Parse SSE base64 protobuf response events.
- Keep fallback tool field numbers aligned with `task.proto`.

## Current Request Shape

Orchids-2api now imports the same generated Go proto module used by the pinned
`warp-proto-apis` revision and marshals a structured
`warp.multi_agent.v1.Request` instead of patching a captured hex template.

Current request construction mirrors upstream's `api::Request` shape:

- `task_context` is present with an empty task list.
- `input.context` includes directory, OS, shell, and current timestamp.
- `input.type = user_inputs`.
- `user_inputs.inputs[].user_query.query` carries the current user turn.
- The handler logs the same `UserQuery.query` preview that the request builder
  marshals, instead of generating a separate local Warp prompt.
- `metadata.conversation_id` is populated only for server-issued Warp
  conversation IDs, not local `chat_*` placeholders.
- `settings.model_config.base` is the requested model.
- `settings.model_config.cli_agent = "cli-agent-auto"`.
- `settings.model_config.computer_use_agent = "computer-use-agent-auto"`.
- `settings.model_config.coding` is left empty because official Warp no longer
  sends this deprecated role field.
- `settings.model_config.base_model_context_window_limit = 0`.
- `settings.supports_parallel_tool_calls = true`
- `settings.supports_reasoning_message = true`
- `settings.web_search_enabled = true`
- `settings.supports_v4a_file_diffs = true`
- `settings.supported_tools` follows upstream local-session defaults for shell,
  file, grep, MCP, subagent, document, prompt-suggestion, apply-diff, and
  search-codebase tools when tools are enabled.
- `settings.supported_cli_agent_tools` follows upstream local-session defaults.
- `mcp_context.servers` groups request-declared tools under an Orchids server.

## Model Discovery and Availability

Official Warp runs with one logged-in user's model configuration. Orchids-2api
runs a multi-account pool, so a model list assembled as a union across accounts
can expose models that a selected account cannot actually call.

Live verification showed that GraphQL visibility is not the same as callability:
`workspace.availableLlms(includeAllConfigurableLlms: true)` can return models
such as Claude/Gemini variants that later fail at `/ai/multi-agent` with errors
like `the requested base model (...) is not allowed for your account` or `No
model available`.

Upstream behavior:

- Fetch feature-specific model choices with `GetFeatureModelChoices`.
- Use `workspaces[].featureModelChoice.agentMode` for agent-mode callable
  choices.
- `workspace.availableLlms(includeAllConfigurableLlms: true)` remains a broad
  catalog surface, not the current agent-mode routing source.

Current behavior:

- Prefer `workspaces[].featureModelChoice.agentMode.choices` as the trusted
  callable model source.
- Keep `auto-open` as the default Warp model.
- Map old auto aliases (`auto`, `auto-efficient`, `auto-genius`) to `auto-open`.
- Retry Warp HTTP 400 model-availability failures once with `auto-open`.
- Classify Warp model-availability errors separately from generic client 400s so
  retry/account-switch policy can treat them as account/model availability, not
  malformed client input.

## Known Differences

- Official `RequestParams` can send separate coding, CLI-agent, computer-use,
  BYO key, custom provider, research-agent, bundled-skills, and orchestration
  settings. Orchids-2api currently uses official default role models and leaves
  those optional advanced settings disabled.
- Official client converts native Warp conversation state into separate
  `UserInputs` and action-result inputs. Orchids-2api receives OpenAI/Claude
  style chat history, so it bridges the current user/tool-result turn into one
  `UserQuery.query` until a full action-result mapper is implemented.
- Official `ResponseEvent.StreamFinished.should_refresh_model_config` tells the
  client when its model config is stale. We now parse and log this signal, but
  do not yet trigger an automatic model refresh.
- Official `StreamFinished` contains request cost and conversation usage
  metadata. We currently keep token usage only.
- Official model refresh is tied to a single user's current model configuration.
  Orchids-2api keeps a per-account model-choice cache because it routes over a
  pooled Warp account set.

## Recommended Next Step

The safer next step is a low-concurrency per-account model probe/cache:

- Probe only selected/default candidate models, not the full configurable
  catalog.
- Cache `(account_id, model_id) -> allowed/unavailable` with a short TTL.
- Use the cache during account selection so a request for a specific model picks
  an account known to allow it.
- Consume `should_refresh_model_config` by scheduling a serialized Warp refresh
  and invalidating the relevant allowedness cache.
- Add a full mapper from incoming tool-result messages to official
  `Request.Input.UserInputs.UserInput.ToolCallResult` when we want tighter
  multi-turn parity with Warp's native conversation state.
