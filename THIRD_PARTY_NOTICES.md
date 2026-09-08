# Third-party code

## chenyme/grok2api

Source: https://github.com/chenyme/grok2api

Pinned commit: `44a390b890e7a3e0dd209b95b8c29a9f2b1be8dd`.

Copyright (c) 2026 Chenyme. Distributed under the MIT license, reproduced in
`licenses/grok2api-MIT.txt`. Retain this notice and license when distributing
these sources or substantial portions of them.

| Local files under internal/grok | Upstream files under backend/internal |
| --- | --- |
| grok2api_streamidle.go, grok2api_streamidle_test.go | infra/provider/cli/semantic_streamidle.go, semantic_streamidle_test.go |
| grok2api_sse.go | infra/provider/cli/responses_response.go (compatibleSSEEvent and consumeCompatibleSSE) |
| handler_image_edits.go | infra/provider/web/image.go (mediaGenInput.imageToImage payload; existing metadata-ID upload transport retained) |

Integration changes: package/import paths, local idle error and buffer constant;
unused service dependencies, SSE SetData method and production TimedOut accessor
are omitted. Timer tests inspect state through a test-only accessor. The quality
classifier, quality retries and their policy tests have been removed to keep the
relay independent of answer quality judgments. Local adapters retain account
ownership, protocol error handling, readable reasoning snapshots and refusals. SSE frame ordering/line endings may be
canonicalized by the upstream codec; data and supported metadata are preserved.
