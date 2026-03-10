package warp

import "testing"

func TestNormalizeToolInputForToolName_ReadRecoversPathFromFallbackField(t *testing.T) {
	t.Parallel()

	got := normalizeToolInputForToolName("Read", `{"field1":"\n8/Users/dailin/Documents/GitHub/TEST/orchids_accounts.txt"}`)
	want := `{"file_path":"/Users/dailin/Documents/GitHub/TEST/orchids_accounts.txt"}`
	if got != want {
		t.Fatalf("normalizeToolInputForToolName(Read) = %q, want %q", got, want)
	}
}

func TestNormalizeToolInputForToolName_ReadKeepsValidPath(t *testing.T) {
	t.Parallel()

	got := normalizeToolInputForToolName("Read", `{"file_path":"/tmp/a.txt","offset":10}`)
	want := `{"file_path":"/tmp/a.txt"}`
	if got != want {
		t.Fatalf("normalizeToolInputForToolName(Read) = %q, want %q", got, want)
	}
}
