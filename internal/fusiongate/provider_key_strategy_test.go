package fusiongate

import (
	"context"
	"testing"
)

func TestProviderKeySelectionModes(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	providerID := insertTestProvider(t, a, "selection-modes", "openai_compatible", "https://example.test", "legacy", 1, 100, "normalized", "any", 0, 3, 30)
	if _, err = a.db.Exec("DELETE FROM provider_api_keys WHERE provider_id=?", providerID); err != nil {
		t.Fatal(err)
	}
	if _, err = a.db.Exec("UPDATE providers SET multi_key_initialized=1 WHERE id=?", providerID); err != nil {
		t.Fatal(err)
	}
	ids := []int64{insertProviderKeyForTest(t, a, providerID, "key-a", "A", "model", providerKeyEgressInherit, nil, 1, 0), insertProviderKeyForTest(t, a, providerID, "key-b", "B", "model", providerKeyEgressInherit, nil, 1, 1), insertProviderKeyForTest(t, a, providerID, "key-c", "C", "model", providerKeyEgressInherit, nil, 1, 2)}
	for i, m := range []float64{2, 1, 2} {
		if _, err = a.db.Exec("UPDATE provider_api_keys SET cost_multiplier=? WHERE id=?", m, ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	check := func(mode string, want ...string) {
		t.Helper()
		if _, err = a.db.Exec("UPDATE providers SET key_selection_mode=? WHERE id=?", mode, providerID); err != nil {
			t.Fatal(err)
		}
		keys, selectErr := a.selectProviderKeys(context.Background(), providerID, "model", nil, nil, true)
		if selectErr != nil {
			t.Fatal(selectErr)
		}
		for i := range want {
			if keys[i].Credential != want[i] {
				t.Fatalf("%s[%d]=%s want %s", mode, i, keys[i].Credential, want[i])
			}
		}
	}
	check(providerKeySelectionConfigured, "key-a", "key-b", "key-c")
	check(providerKeySelectionLowMultiplier, "key-b", "key-a", "key-c")
	check(providerKeySelectionHighMultiplier, "key-a", "key-c", "key-b")
	if _, err = a.db.Exec("UPDATE providers SET key_selection_mode=? WHERE id=?", providerKeySelectionRoundRobin, providerID); err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"key-a", "key-b", "key-c", "key-a"} {
		key, selectErr := a.selectProviderKey(context.Background(), providerID, "model", nil, nil, true)
		if selectErr != nil {
			t.Fatal(selectErr)
		}
		if key.Credential != want {
			t.Fatalf("round robin %d=%s want %s", i, key.Credential, want)
		}
	}
}
