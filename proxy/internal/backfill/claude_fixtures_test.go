package backfill

import "testing"

func TestScanClaudeRetainedCLIAndDesktop(t *testing.T) {
	// Real archived CLI and Desktop metadata, reduced to accounting fields
	// with replaced identifiers. The resumed sessions have two cumulative
	// summaries; the CLI tool turn repeats one request in multiple frames.
	preview := claudeFixtureScan(t, "testdata/claude", Window{}, DesktopOwnership{})
	want := map[string][4]int64{
		"req_CliResume1": {2, 22, 0, 608},
		"req_CliResume2": {2, 21, 0, 705},
		"req_CliTool1":   {2, 83, 0, 1700},
		"req_CliTool2":   {2, 20, 1700, 163},
		"req_Desktop1":   {2, 21, 46453, 24422},
		"req_Desktop2":   {2, 21, 58251, 12710},
	}
	for _, row := range preview.Records {
		got := [4]int64{row.Input, row.Output, row.CacheRead, row.CacheWrite}
		if expected, ok := want[row.ID]; !ok || got != expected {
			t.Fatalf("unexpected usage for %s: %v (expected %v)", row.ID, got, expected)
		}
		delete(want, row.ID)
	}
	if len(want) > 0 {
		t.Fatalf("missing requests %v, exclusions: %v", want, preview.Excluded)
	}
}
