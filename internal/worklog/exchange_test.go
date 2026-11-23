package worklog

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestLedgerExchangeRoundTrip(t *testing.T) {
	root := filepath.Join(t.TempDir(), "synthetic-root")
	other := filepath.Join(t.TempDir(), "other-root")
	cp := checkpointFixture()
	cp.Operations = []Operation{{ID: "verify", InputFingerprint: "two words", Status: "verified", Evidence: "a path/report.json"}}
	body, err := RenderLedger(root, 17, cp)
	if err != nil {
		t.Fatal(err)
	}
	got, generation, err := DecodeLedger(body, root)
	if err != nil || generation != 17 || !reflect.DeepEqual(got, cp) {
		t.Fatalf("round trip: %+v %d %v", got, generation, err)
	}
	if _, _, err := DecodeLedger(body, other); err == nil {
		t.Fatal("relocated ledger silently adopted")
	}
}
