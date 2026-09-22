package grok

import (
	"bytes"
	"strings"
	"testing"

	"orchids-api/internal/store"
)

func TestAnthropicStreamMessageStartCarriesEstimatedInput(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n"
	var out bytes.Buffer
	if err := translateOpenAIChatStreamToAnthropicWithInput(&out, strings.NewReader(stream), "grok-4.6", 7); err != nil {
		t.Fatal(err)
	}
	first := strings.Split(out.String(), "\n\n")[0]
	if !strings.Contains(first, `"input_tokens":7`) || !strings.Contains(first, `"cache_read_input_tokens":0`) {
		t.Fatalf("message_start usage = %s", first)
	}
}

func TestApplyCLIBillingDerivesPercentFromMonthly(t *testing.T) {
	info := &CLIBillingInfo{MonthlyLimit: 200, MonthlyUsed: 50, HasMonthly: true}
	acc := &store.Account{}
	ApplyCLIBillingInfo(acc, info)
	if !acc.GrokBilling.Weekly.HasUsage || acc.GrokBilling.Weekly.UsagePercent != 25 {
		t.Fatalf("derived percent not persisted: %+v", acc.GrokBilling)
	}
}
