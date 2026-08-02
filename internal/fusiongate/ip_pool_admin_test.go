package fusiongate

import (
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
	if deleteRecorder.Code != http.StatusConflict {
		t.Fatalf("delete in-use node status=%d body=%s", deleteRecorder.Code, deleteRecorder.Body.String())
	}

	unbind := httptest.NewRequest(http.MethodPatch, "/api/admin/providers/"+intString(providerID), strings.NewReader(`{"ip_pool_node_id":0}`))
	unbindRecorder := httptest.NewRecorder()
	a.providerByID(unbindRecorder, unbind, adminCtx{})
	if unbindRecorder.Code != http.StatusOK {
		t.Fatalf("unbind provider status=%d body=%s", unbindRecorder.Code, unbindRecorder.Body.String())
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM providers WHERE id=? AND ip_pool_node_id IS NULL`, providerID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("provider did not return to direct mode: count=%d err=%v", count, err)
	}

	deleteRecorder = httptest.NewRecorder()
	a.ipPoolNodeByID(deleteRecorder, httptest.NewRequest(http.MethodDelete, "/api/admin/ip-pool/"+intString(created.ID), nil), adminCtx{})
	if deleteRecorder.Code != http.StatusOK {
		t.Fatalf("delete unused node status=%d body=%s", deleteRecorder.Code, deleteRecorder.Body.String())
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
