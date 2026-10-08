package fusiongate

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIPPoolNodeCRUDAndProviderBinding(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	// Keep this API test independent of the host sing-box installation. A
	// disabled node is still assignable after it is explicitly enabled below,
	// while reconciliation has no active process to start during creation.
	create := httptest.NewRequest(http.MethodPost, "/api/admin/ip-pool", strings.NewReader(`{
		"name":"US Reality",
		"share_link":"vless://bf000d23-0752-40b4-affe-68f7707a9661@reality.example.com:443?security=reality&sni=www.microsoft.com&fp=chrome&pbk=jNXHt1yRo0vDuchQlIP6Z0ZvjT3KtzVI-T4E7RoLJS0&sid=0123456789abcdef&flow=xtls-rprx-vision&type=tcp",
		"enabled":false
	}`))
	createdRecorder := httptest.NewRecorder()
	a.ipPoolNodes(createdRecorder, create, adminCtx{})
	if createdRecorder.Code != http.StatusCreated {
		t.Fatalf("create node status=%d body=%s", createdRecorder.Code, createdRecorder.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(createdRecorder.Body.Bytes(), &created); err != nil || created.ID < 1 {
		t.Fatalf("decode created node: id=%d err=%v", created.ID, err)
	}

	listRecorder := httptest.NewRecorder()
	a.ipPoolNodes(listRecorder, httptest.NewRequest(http.MethodGet, "/api/admin/ip-pool", nil), adminCtx{})
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list node status=%d body=%s", listRecorder.Code, listRecorder.Body.String())
	}
	body := listRecorder.Body.String()
	for _, secret := range []string{"bf000d23-0752-40b4-affe-68f7707a9661", "jNXHt1yRo0vDuchQlIP6Z0ZvjT3KtzVI-T4E7RoLJS0"} {
		if strings.Contains(body, secret) {
			t.Fatalf("node list exposed sensitive share-link material: %s", body)
		}
	}

	// Enable it directly for assignment without starting a real external
	// process; the validation path intentionally only accepts enabled nodes.
	if _, err := a.db.Exec(`UPDATE ip_pool_nodes SET enabled=1 WHERE id=?`, created.ID); err != nil {
		t.Fatal(err)
	}
	providerID := insertTestProvider(t, a, "bound-provider", "openai_compatible", "https://example.test", "secret", 1, 100, "normalized", "any", 0, 3, 30)
	patch := httptest.NewRequest(http.MethodPatch, "/api/admin/providers/"+intString(providerID), strings.NewReader(`{"ip_pool_node_id":`+intString(created.ID)+`}`))
	patchRecorder := httptest.NewRecorder()
	a.providerByID(patchRecorder, patch, adminCtx{})
	if patchRecorder.Code != http.StatusOK {
		t.Fatalf("bind provider status=%d body=%s", patchRecorder.Code, patchRecorder.Body.String())
	}
	var selected int64
	if err := a.db.QueryRow(`SELECT ip_pool_node_id FROM providers WHERE id=?`, providerID).Scan(&selected); err != nil || selected != created.ID {
		t.Fatalf("provider node=%d err=%v", selected, err)
	}

	deleteRecorder := httptest.NewRecorder()
	a.ipPoolNodeByID(deleteRecorder, httptest.NewRequest(http.MethodDelete, "/api/admin/ip-pool/"+intString(created.ID), nil), adminCtx{})
	if deleteRecorder.Code != http.StatusOK {
		t.Fatalf("delete bound node status=%d body=%s", deleteRecorder.Code, deleteRecorder.Body.String())
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM providers WHERE id=? AND ip_pool_node_id IS NULL`, providerID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("provider did not return to direct mode: count=%d err=%v", count, err)
	}
	if recorder := deleteIPPoolNodeForTest(a, created.ID); recorder.Code != http.StatusNotFound {
		t.Fatalf("delete missing node status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestProvidersListIncludesMaskedIPPoolAssignment(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	link, err := a.encrypt("socks5://user:secret@proxy.example.com:1080")
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.db.Exec(`INSERT INTO ip_pool_nodes(name,protocol,server,share_link,enabled,local_port,status,created_at,updated_at) VALUES('Proxy A','socks5','proxy.example.com:1080',?,0,22000,'pending',?,?)`, link, now(), now())
	if err != nil {
		t.Fatal(err)
	}
	nodeID, _ := result.LastInsertId()
	providerID := insertTestProvider(t, a, "listed-provider", "openai_compatible", "https://example.test", "secret", 1, 100, "normalized", "any", 0, 3, 30)
	if _, err := a.db.Exec(`UPDATE providers SET ip_pool_node_id=? WHERE id=?`, nodeID, providerID); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	a.providers(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/providers", nil), adminCtx{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("providers status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, expected := range []string{`"ip_pool_node_id":` + intString(nodeID), `"ip_pool_node_name":"Proxy A"`, `"ip_pool_node_protocol":"socks5"`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("providers response missing %s: %s", expected, body)
		}
	}
	if strings.Contains(body, "user:secret") {
		t.Fatalf("providers response exposed proxy credential: %s", body)
	}
}

func insertIPPoolNodeForTest(t *testing.T, a *App, name string, port int) int64 {
	t.Helper()
	link, err := a.encrypt("socks5://user:secret@proxy.example.com:1080")
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.db.Exec(`INSERT INTO ip_pool_nodes(name,protocol,server,share_link,enabled,local_port,status,created_at,updated_at) VALUES(?,'socks5','proxy.example.com:1080',?,0,?,'pending',?,?)`, name, link, port, now(), now())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	return id
}

func deleteIPPoolNodeForTest(a *App, id int64) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	a.ipPoolNodeByID(recorder, httptest.NewRequest(http.MethodDelete, "/api/admin/ip-pool/"+intString(id), nil), adminCtx{})
	return recorder
}

func TestIPPoolNodeDeletionReleasesAssignments(t *testing.T) {
	a, err := New(testConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	node := insertIPPoolNodeForTest(t, a, "Deleted", 23001)
	other := insertIPPoolNodeForTest(t, a, "Retained", 23002)
	for _, state := range []struct {
		name              string
		enabled, archived int
	}{{"active", 1, 0}, {"disabled", 0, 0}, {"archived", 0, 1}} {
		p := insertTestProvider(t, a, state.name, "openai_compatible", "https://example.test", "secret", 1, 100, "normalized", "any", 0, 3, 30)
		if _, err := a.db.Exec(`UPDATE providers SET ip_pool_node_id=?,enabled=?,archived=? WHERE id=?`, node, state.enabled, state.archived, p); err != nil {
			t.Fatal(err)
		}
		for i, mode := range []string{providerKeyEgressNode, providerKeyEgressInherit, providerKeyEgressDirect} {
			insertProviderKeyForTest(t, a, p, "sk-"+mode, mode, "", mode, node, state.enabled, i)
		}
		insertProviderKeyForTest(t, a, p, "sk-other", "other", "", providerKeyEgressNode, other, 1, 3)
	}
	// A pinned Key must become direct, not inherit a different channel proxy.
	p := insertTestProvider(t, a, "other-default", "openai_compatible", "https://example.test", "secret", 1, 100, "normalized", "any", 0, 3, 30)
	if _, err := a.db.Exec(`UPDATE providers SET ip_pool_node_id=? WHERE id=?`, other, p); err != nil {
		t.Fatal(err)
	}
	key := insertProviderKeyForTest(t, a, p, "sk-pinned", "pinned", "", providerKeyEgressNode, node, 1, 0)
	if r := deleteIPPoolNodeForTest(a, node); r.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", r.Code, r.Body.String())
	}
	for _, state := range []struct {
		name              string
		enabled, archived int
	}{{"active", 1, 0}, {"disabled", 0, 0}, {"archived", 0, 1}} {
		var id int64
		var ref sql.NullInt64
		var enabled, archived int
		if err := a.db.QueryRow(`SELECT id,ip_pool_node_id,enabled,archived FROM providers WHERE name=?`, state.name).Scan(&id, &ref, &enabled, &archived); err != nil {
			t.Fatal(err)
		}
		if ref.Valid || enabled != state.enabled || archived != state.archived {
			t.Fatalf("channel %s changed incorrectly", state.name)
		}
		for _, name := range []string{providerKeyEgressNode, providerKeyEgressInherit, providerKeyEgressDirect, "other"} {
			var mode string
			var keyEnabled int
			if err := a.db.QueryRow(`SELECT egress_mode,ip_pool_node_id,enabled FROM provider_api_keys WHERE provider_id=? AND name=?`, id, name).Scan(&mode, &ref, &keyEnabled); err != nil {
				t.Fatal(err)
			}
			want := name
			if name == providerKeyEgressNode {
				want = providerKeyEgressDirect
			}
			if name == "other" {
				if mode != providerKeyEgressNode || !ref.Valid || ref.Int64 != other || keyEnabled != 1 {
					t.Fatal("unrelated Key changed")
				}
				continue
			}
			if mode != want || ref.Valid || keyEnabled != state.enabled {
				t.Fatalf("Key %s changed incorrectly", name)
			}
		}
	}
	var mode string
	var ref sql.NullInt64
	if err := a.db.QueryRow(`SELECT egress_mode,ip_pool_node_id FROM provider_api_keys WHERE id=?`, key).Scan(&mode, &ref); err != nil {
		t.Fatal(err)
	}
	if mode != providerKeyEgressDirect || ref.Valid {
		t.Fatal("pinned Key inherited another proxy")
	}
	var remaining int64
	if err := a.db.QueryRow(`SELECT ip_pool_node_id FROM providers WHERE id=?`, p).Scan(&remaining); err != nil || remaining != other {
		t.Fatalf("unrelated channel changed: %v", err)
	}
	var count int
	if err := a.db.QueryRow(`SELECT count(*) FROM ip_pool_nodes WHERE id=?`, node).Scan(&count); err != nil || count != 0 {
		t.Fatalf("node remains: %v", err)
	}
}

func TestIPPoolNodeDeletionRollsBackAssignments(t *testing.T) {
	for _, stage := range []string{"keys", "node"} {
		t.Run(stage, func(t *testing.T) {
			a, err := New(testConfig(t))
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			node := insertIPPoolNodeForTest(t, a, "Rollback", 23001)
			p := insertTestProvider(t, a, "bound", "openai_compatible", "https://example.test", "secret", 1, 100, "normalized", "any", 0, 3, 30)
			if _, err := a.db.Exec(`UPDATE providers SET ip_pool_node_id=? WHERE id=?`, node, p); err != nil {
				t.Fatal(err)
			}
			key := insertProviderKeyForTest(t, a, p, "sk-rollback", "pinned", "", providerKeyEgressNode, node, 1, 0)
			trigger := `CREATE TRIGGER reject_delete BEFORE DELETE ON ip_pool_nodes BEGIN SELECT RAISE(ABORT,'test failure'); END`
			if stage == "keys" {
				trigger = `CREATE TRIGGER reject_update BEFORE UPDATE ON provider_api_keys BEGIN SELECT RAISE(ABORT,'test failure'); END`
			}
			if _, err := a.db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			if r := deleteIPPoolNodeForTest(a, node); r.Code != http.StatusInternalServerError {
				t.Fatalf("status=%d body=%s", r.Code, r.Body.String())
			}
			var ref int64
			var mode string
			if err := a.db.QueryRow(`SELECT ip_pool_node_id FROM providers WHERE id=?`, p).Scan(&ref); err != nil || ref != node {
				t.Fatalf("channel not rolled back: %v", err)
			}
			if err := a.db.QueryRow(`SELECT ip_pool_node_id,egress_mode FROM provider_api_keys WHERE id=?`, key).Scan(&ref, &mode); err != nil || ref != node || mode != providerKeyEgressNode {
				t.Fatalf("Key not rolled back: %v", err)
			}
			if err := a.db.QueryRow(`SELECT id FROM ip_pool_nodes WHERE id=?`, node).Scan(&ref); err != nil {
				t.Fatalf("node not rolled back: %v", err)
			}
		})
	}
}
