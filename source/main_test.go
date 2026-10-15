package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) *App {
	t.Helper()
	a, e := newApp(t.TempDir(), "../", false)
	if e != nil {
		t.Fatal(e)
	}
	return a
}
func request(a *App, method, path string, b []byte, token string, lan bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://127.0.0.1:8564"+path, bytes.NewReader(b))
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("X-Healthtara-Token", token)
	}
	w := httptest.NewRecorder()
	a.handler(lan).ServeHTTP(w, r)
	return w
}
func packageFixture() *ImportPackage {
	return &ImportPackage{Schema: "healthtara-import-v1", Origin: "test.xlsx", Date: "2026-10-13", SHA: strings.Repeat("a", 64), Hospitals: []map[string]any{{"code": "1234567", "name": "Rumah Sakit Uji", "province": "Aceh", "completeness": float64(.5), "serviceCount": float64(1)}}, Details: map[string]json.RawMessage{"1234567": json.RawMessage(`{"code":"1234567","profile":{},"services":[]}`)}}
}
func TestSnapshotIntegrity(t *testing.T) {
	a := fixture(t)
	if len(a.ids) != 3323 {
		t.Fatalf("hospital count %d", len(a.ids))
	}
	if len(a.detailEntries) != 3323 {
		t.Fatalf("detail count %d", len(a.detailEntries))
	}
	for code := range a.ids {
		if _, e := a.embeddedDetail(code); e != nil {
			t.Fatal(code, e)
		}
	}
	var core map[string]json.RawMessage
	json.Unmarshal(a.coreBytes(), &core)
	var province []any
	json.Unmarshal(core["province"], &province)
	if len(province) != 318 {
		t.Fatalf("province observations %d", len(province))
	}
}
func TestHTTPAccessControls(t *testing.T) {
	a := fixture(t)
	for _, path := range []string{"/api/ping", "/api/meta", "/api/core", "/api/reference", "/api/hospital/1107017", "/", "/app.js"} {
		if w := request(a, "GET", path, nil, "", false); w.Code != 200 {
			t.Fatalf("%s status %d %s", path, w.Code, w.Body.String())
		}
	}
	if w := request(a, "GET", "/api/bugs", nil, "", false); w.Code != 403 {
		t.Fatal("unauthenticated bug read allowed")
	}
	if w := request(a, "POST", "/api/config", []byte(`{"publicURL":"https://example.com"}`), a.token, false); w.Code != 200 {
		t.Fatal(w.Code, w.Body)
	}
	r := httptest.NewRequest("POST", "http://127.0.0.1:8564/api/config", strings.NewReader(`{}`))
	r.RemoteAddr = "127.0.0.1:123"
	r.Header.Set("X-Healthtara-Token", a.token)
	r.Header.Set("Origin", "https://malicious.example")
	w := httptest.NewRecorder()
	a.handler(false).ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross-origin write accepted")
	}
	r = httptest.NewRequest("GET", "http://malicious.example/api/meta", nil)
	r.RemoteAddr = "127.0.0.1:12"
	w = httptest.NewRecorder()
	a.handler(false).ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("rebinding host accepted")
	}
	for _, path := range []string{"/api/hospital/../../../x", "/data/hospital_details.zip", "/data/core.json.gz", "/api/unknown"} {
		if w := request(a, "GET", path, nil, "", false); w.Code == 200 {
			t.Fatalf("private/unexpected path served: %s", path)
		}
	}
}
func TestLANReadOnly(t *testing.T) {
	a := fixture(t)
	r := httptest.NewRequest("GET", "http://192.168.1.2/api/meta", nil)
	r.RemoteAddr = "192.168.1.50:345"
	r.AddCookie(&http.Cookie{Name: "healthtara_view", Value: a.lanToken})
	w := httptest.NewRecorder()
	a.handler(true).ServeHTTP(w, r)
	var m map[string]any
	json.Unmarshal(w.Body.Bytes(), &m)
	if w.Code != 200 || m["isAdmin"] != false || m["token"] != nil || m["storagePath"] != nil {
		t.Fatal("LAN metadata leaks authority", w.Code, m)
	}
	r = httptest.NewRequest("POST", "http://192.168.1.2/api/import", strings.NewReader(`{}`))
	r.RemoteAddr = "192.168.1.50:345"
	r.AddCookie(&http.Cookie{Name: "healthtara_view", Value: a.lanToken})
	r.Header.Set("X-Healthtara-Token", a.token)
	w = httptest.NewRecorder()
	a.handler(true).ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("LAN write accepted")
	}
	if w = request(a, "GET", "/api/core", nil, "", true); w.Code != 403 {
		t.Fatal("LAN accepted missing share cookie")
	}
}
func TestImportValidationAndRecovery(t *testing.T) {
	a := fixture(t)
	p := packageFixture()
	if e := validateImport(p); e != nil {
		t.Fatal(e)
	}
	p.Hospitals = append(p.Hospitals, p.Hospitals[0])
	if e := validateImport(p); e == nil {
		t.Fatal("duplicate code accepted")
	}
	p = packageFixture()
	p.Hospitals[0]["website"] = "javascript:alert(1)"
	if e := validateImport(p); e == nil {
		t.Fatal("unsafe URL accepted")
	}
	p = packageFixture()
	p.Hospitals[0]["completeness"] = float64(2)
	if e := validateImport(p); e == nil {
		t.Fatal("invalid completeness accepted")
	}
	p = packageFixture()
	if e := a.saveImport(p); e != nil {
		t.Fatal(e)
	}
	if len(a.ids) != 1 || !a.ids["1234567"] {
		t.Fatal("import did not activate")
	}
	if e := a.saveImport(packageFixture()); e != nil {
		t.Fatal(e)
	}
	entries, _ := os.ReadDir(filepath.Join(a.dir, "backups"))
	if len(entries) != 1 {
		t.Fatal("prior import not backed up")
	}
	a2, e := newApp(a.dir, a.root, false)
	if e != nil || len(a2.ids) != 1 {
		t.Fatal("import not persistent", e)
	}
	w := request(a, "POST", "/api/restore", []byte(`{"name":"bundled"}`), a.token, false)
	if w.Code != 200 || len(a.ids) != 3323 {
		t.Fatal("bundled restore failed", w.Code, w.Body)
	}
	w = request(a, "POST", "/api/restore", []byte(`{"name":"../../dataset.json.gz"}`), a.token, false)
	if w.Code != 400 {
		t.Fatal("unsafe backup path accepted")
	}
	if w = request(a, "GET", "/api/hospital/1234567", nil, "", false); w.Code != 404 {
		t.Fatal("retired import detail still active")
	}
}
func TestBugPersistenceDedupAndScrubbing(t *testing.T) {
	a := fixture(t)
	b := Bug{Title: "Error key token=secret-value", Page: "map", Automatic: true, Status: "Baru"}
	one, e := a.saveBug(b)
	if e != nil {
		t.Fatal(e)
	}
	two, e := a.saveBug(b)
	if e != nil || one.ID != two.ID || two.Count != 2 {
		t.Fatal("automatic dedup failed", e)
	}
	if strings.Contains(two.Title, "secret-value") {
		t.Fatal("secret not scrubbed")
	}
	two.Status = "Selesai"
	if _, e = a.saveBug(two); e != nil {
		t.Fatal(e)
	}
	a2, e := newApp(a.dir, a.root, false)
	if e != nil || len(a2.bugs) != 1 || a2.bugs[0].Status != "Selesai" {
		t.Fatal("bug persistence failed", e)
	}
	w := request(a, "GET", "/api/diagnostics", nil, a.token, false)
	if w.Code != 200 || strings.Contains(w.Body.String(), a.token) {
		t.Fatal("diagnostics secret or error")
	}
}
func TestCloudAdmin(t *testing.T) {
	t.Setenv("HEALTHTARA_ADMIN_KEY", "a-test-admin-passphrase")
	a, e := newApp(t.TempDir(), "../", true)
	if e != nil {
		t.Fatal(e)
	}
	w := request(a, "GET", "/api/meta", nil, "", false)
	var m map[string]any
	json.Unmarshal(w.Body.Bytes(), &m)
	if m["isAdmin"] != false || m["token"] != nil {
		t.Fatal("cloud grants loopback automatic admin")
	}
	w = request(a, "POST", "/api/login", []byte(`{"key":"a-test-admin-passphrase"}`), "", false)
	var v map[string]string
	json.Unmarshal(w.Body.Bytes(), &v)
	if w.Code != 200 || v["token"] == "" {
		t.Fatal("cloud login failed")
	}
	w = request(a, "GET", "/api/bugs", nil, v["token"], false)
	if w.Code != 200 {
		t.Fatal("cloud session not accepted")
	}
	w = request(a, "POST", "/api/quit", []byte(`{}`), v["token"], false)
	if w.Code != 403 {
		t.Fatal("cloud quit allowed")
	}
}
