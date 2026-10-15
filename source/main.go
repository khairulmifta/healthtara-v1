package main

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed web
var assets embed.FS

const version = "1.0.0"
const appID = "healthtara-v1"
const importLimit = 128 << 20

type Config struct {
	PublicURL string `json:"publicURL"`
}
type Bug struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Page        string `json:"page"`
	Severity    string `json:"severity"`
	Status      string `json:"status"`
	Steps       string `json:"steps"`
	Expected    string `json:"expected"`
	Actual      string `json:"actual"`
	Created     string `json:"created"`
	Updated     string `json:"updated"`
	Version     string `json:"version"`
	Browser     string `json:"browser"`
	Automatic   bool   `json:"automatic"`
	Count       int    `json:"count"`
	Fingerprint string `json:"fingerprint"`
}
type ImportPackage struct {
	Schema    string                     `json:"schema"`
	Origin    string                     `json:"origin"`
	Date      string                     `json:"date"`
	SHA       string                     `json:"sha256"`
	Hospitals []map[string]any           `json:"hospitals"`
	Details   map[string]json.RawMessage `json:"details"`
	Reference map[string]json.RawMessage `json:"reference,omitempty"`
}
type App struct {
	mu            sync.RWMutex
	dir           string
	root          string
	cloud         bool
	token         string
	adminKey      string
	port          int
	lanPort       int
	lanToken      string
	lan           *http.Server
	stop          chan struct{}
	config        Config
	bugs          []Bug
	current       *ImportPackage
	base          map[string]json.RawMessage
	ids           map[string]bool
	sessions      map[string]time.Time
	loginAttempts map[string][]time.Time
	detailArchive *zip.Reader
	detailEntries map[string]*zip.File
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func unzipJSON(name string) ([]byte, error) {
	b, e := assets.ReadFile(name)
	if e != nil {
		return nil, e
	}
	r, e := gzip.NewReader(bytes.NewReader(b))
	if e != nil {
		return nil, e
	}
	defer r.Close()
	return io.ReadAll(r)
}
func writeAtomic(name string, b []byte) error {
	if e := os.MkdirAll(filepath.Dir(name), 0700); e != nil {
		return e
	}
	tmp := name + ".tmp"
	f, e := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if e != nil {
		return e
	}
	if _, e = f.Write(b); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	return os.Rename(tmp, name)
}
func writeJSON(name string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	return writeAtomic(name, b)
}
func gzipData(b []byte) []byte {
	var buf bytes.Buffer
	z := gzip.NewWriter(&buf)
	z.Write(b)
	z.Close()
	return buf.Bytes()
}
func readImport(name string) (*ImportPackage, error) {
	b, e := os.ReadFile(name)
	if e != nil {
		return nil, e
	}
	z, e := gzip.NewReader(bytes.NewReader(b))
	if e != nil {
		return nil, e
	}
	defer z.Close()
	var p ImportPackage
	e = json.NewDecoder(io.LimitReader(z, importLimit+1)).Decode(&p)
	if e != nil {
		return nil, e
	}
	if e = validateImport(&p); e != nil {
		return nil, e
	}
	return &p, nil
}
func defaultDir() string {
	if p := os.Getenv("HEALTHTARA_DATA_DIR"); p != "" {
		return p
	}
	p, e := os.UserConfigDir()
	if e != nil {
		p = os.TempDir()
	}
	return filepath.Join(p, "Healthtara", "v1")
}
func newApp(dir, root string, cloud bool) (*App, error) {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	a := &App{dir: dir, root: root, cloud: cloud, token: randomToken(), lanToken: randomToken(), adminKey: os.Getenv("HEALTHTARA_ADMIN_KEY"), stop: make(chan struct{}), sessions: map[string]time.Time{}, loginAttempts: map[string][]time.Time{}}
	raw, e := unzipJSON("web/data/core.json.gz")
	if e != nil {
		return nil, e
	}
	if e = json.Unmarshal(raw, &a.base); e != nil {
		return nil, e
	}
	if b, e := os.ReadFile(filepath.Join(dir, "config.json")); e == nil {
		json.Unmarshal(b, &a.config)
	}
	if b, e := os.ReadFile(filepath.Join(dir, "bugs.json")); e == nil {
		if e = json.Unmarshal(b, &a.bugs); e != nil {
			log.Printf("Bug store not readable: %v", e)
			return nil, fmt.Errorf("bugs.json tidak valid; salin backup lalu perbaiki: %w", e)
		}
	}
	if a.bugs == nil {
		a.bugs = []Bug{}
	}
	if p, e := readImport(filepath.Join(dir, "dataset.json.gz")); e == nil {
		a.current = p
	} else if !os.IsNotExist(e) {
		return nil, fmt.Errorf("dataset pembaruan rusak: %w", e)
	}
	packed, e := assets.ReadFile("web/data/hospital_details.zip")
	if e != nil {
		return nil, e
	}
	a.detailArchive, e = zip.NewReader(bytes.NewReader(packed), int64(len(packed)))
	if e != nil {
		return nil, e
	}
	a.detailEntries = map[string]*zip.File{}
	for _, f := range a.detailArchive.File {
		a.detailEntries[f.Name] = f
	}
	a.rebuildIDs()
	return a, nil
}
func (a *App) embeddedDetail(code string) ([]byte, error) {
	f, ok := a.detailEntries[code+".json.gz"]
	if !ok {
		return nil, os.ErrNotExist
	}
	r, e := f.Open()
	if e != nil {
		return nil, e
	}
	defer r.Close()
	return io.ReadAll(r)
}
func (a *App) rebuildIDs() {
	a.ids = map[string]bool{}
	var rows []map[string]any
	if a.current != nil {
		rows = a.current.Hospitals
	} else {
		json.Unmarshal(a.base["hospitals"], &rows)
	}
	for _, r := range rows {
		a.ids[fmt.Sprint(r["code"])] = true
	}
}
func loopback(r *http.Request) bool {
	host, _, e := net.SplitHostPort(r.RemoteAddr)
	return e == nil && net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}
func equal(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
func (a *App) admin(r *http.Request, lan bool) bool {
	if lan {
		return false
	}
	t := r.Header.Get("X-Healthtara-Token")
	if !a.cloud {
		return loopback(r) && equal(t, a.token)
	}
	a.mu.RLock()
	expiry, ok := a.sessions[t]
	a.mu.RUnlock()
	return ok && time.Now().Before(expiry)
}
func (a *App) needAdmin(w http.ResponseWriter, r *http.Request, lan bool) bool {
	if !a.admin(r, lan) {
		http.Error(w, "Akses administrator diperlukan", 403)
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, e := url.Parse(o)
		if e != nil || u.Host != r.Host {
			http.Error(w, "Origin ditolak", 403)
			return false
		}
	}
	return true
}
func jsonReply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}
func decode(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	d := json.NewDecoder(r.Body)
	if e := d.Decode(v); e != nil {
		http.Error(w, "Format atau ukuran berkas tidak valid: "+e.Error(), 400)
		return false
	}
	var extra any
	if e := d.Decode(&extra); e != io.EOF {
		http.Error(w, "JSON berisi data tambahan", 400)
		return false
	}
	return true
}
func textField(m map[string]any, key string) string { s, _ := m[key].(string); return s }

var codePattern = regexp.MustCompile(`^[0-9]{5,12}$`)

func validateImport(p *ImportPackage) error {
	if p.Schema != "healthtara-import-v1" {
		return errors.New("Skema pembaruan tidak dikenal")
	}
	if len(p.Hospitals) == 0 || len(p.Hospitals) > 30000 {
		return errors.New("Jumlah RS di luar batas 1 sampai 30000")
	}
	if len(p.Origin) > 240 || len(p.Date) > 40 || len(p.SHA) != 64 {
		return errors.New("Identitas sumber/sha256 tidak valid")
	}
	ids := map[string]bool{}
	for _, h := range p.Hospitals {
		c := textField(h, "code")
		if !codePattern.MatchString(c) || ids[c] {
			return errors.New("Kode RS kosong, tidak valid, atau duplikat: " + c)
		}
		ids[c] = true
		if strings.TrimSpace(textField(h, "name")) == "" || strings.TrimSpace(textField(h, "province")) == "" {
			return errors.New("Nama dan provinsi wajib diisi: " + c)
		}
		for _, key := range []string{"website", "source", "maps", "websiteSource"} {
			if v := textField(h, key); v != "" {
				u, e := url.Parse(v)
				if e != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
					return errors.New("URL sumber tidak valid pada " + c)
				}
			}
		}
		for _, key := range []string{"bedRowsTotal", "staffRowsTotal", "serviceCount"} {
			if v, ok := h[key].(float64); ok && (v < 0 || v > 1e8) {
				return errors.New("Angka negatif/tidak masuk rentang pada " + c)
			}
		}
		if v, ok := h["completeness"].(float64); ok && (v < 0 || v > 1) {
			return errors.New("Kelengkapan harus 0 sampai 1")
		}
		detail, ok := p.Details[c]
		if !ok || !json.Valid(detail) {
			return errors.New("Rincian RS tidak lengkap: " + c)
		}
		var d map[string]json.RawMessage
		json.Unmarshal(detail, &d)
		var id string
		json.Unmarshal(d["code"], &id)
		if id != c {
			return errors.New("Kode detail berbeda: " + c)
		}
	}
	if len(p.Details) != len(ids) {
		return errors.New("Jumlah kode detail tidak sama dengan profil")
	}
	return nil
}
func (a *App) coreBytes() []byte {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make(map[string]json.RawMessage, len(a.base))
	for k, v := range a.base {
		out[k] = v
	}
	if a.current != nil {
		out["hospitals"], _ = json.Marshal(a.current.Hospitals)
		out["origin"], _ = json.Marshal(a.current.Origin)
		out["dataDate"], _ = json.Marshal(a.current.Date)
		out["originSHA256"], _ = json.Marshal(a.current.SHA)
		out["snapshotNote"], _ = json.Marshal("Dataset RS diimpor pengguna. Seri provinsi tetap memakai sumber historis v3.")
	}
	b, _ := json.Marshal(out)
	return b
}
func (a *App) serveJSONData(w http.ResponseWriter, r *http.Request, b []byte, compressed bool) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		if !compressed {
			b = gzipData(b)
		}
		w.Header().Set("Content-Encoding", "gzip")
	} else if compressed {
		z, e := gzip.NewReader(bytes.NewReader(b))
		if e != nil {
			http.Error(w, "Data gagal dibaca", 500)
			return
		}
		b, _ = io.ReadAll(z)
		z.Close()
	}
	w.Write(b)
}
func (a *App) backupCurrent() error {
	if a.current == nil {
		return nil
	}
	b, e := json.Marshal(a.current)
	if e != nil {
		return e
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	return writeAtomic(filepath.Join(a.dir, "backups", stamp+"_dataset.json.gz"), gzipData(b))
}
func (a *App) saveImport(p *ImportPackage) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.backupCurrent(); e != nil {
		return e
	}
	b, e := json.Marshal(p)
	if e != nil {
		return e
	}
	if e = writeAtomic(filepath.Join(a.dir, "dataset.json.gz"), gzipData(b)); e != nil {
		return e
	}
	a.current = p
	a.rebuildIDs()
	return nil
}
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

var secretPattern = regexp.MustCompile(`(?i)(token|secret|api[_-]?key|password|share)([=:][^\s&]+)`)

func scrub(s string) string { return secretPattern.ReplaceAllString(s, "$1=[disamarkan]") }
func (a *App) saveBug(b Bug) (Bug, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339)
	b.Title = truncate(scrub(strings.TrimSpace(b.Title)), 220)
	if b.Title == "" {
		return b, errors.New("Judul laporan harus diisi")
	}
	b.Steps = truncate(scrub(b.Steps), 6000)
	b.Expected = truncate(scrub(b.Expected), 3000)
	b.Actual = truncate(scrub(b.Actual), 6000)
	b.Page = truncate(b.Page, 80)
	b.Browser = truncate(b.Browser, 300)
	b.Version = version
	b.Updated = now
	if b.Severity != "Rendah" && b.Severity != "Sedang" && b.Severity != "Tinggi" {
		b.Severity = "Sedang"
	}
	if b.Status != "Baru" && b.Status != "Dikerjakan" && b.Status != "Selesai" {
		b.Status = "Baru"
	}
	if b.ID != "" {
		for i, old := range a.bugs {
			if old.ID == b.ID {
				b.Created = old.Created
				b.Count = old.Count
				b.Automatic = old.Automatic
				b.Fingerprint = old.Fingerprint
				next := append([]Bug(nil), a.bugs...)
				next[i] = b
				if e := writeJSON(filepath.Join(a.dir, "bugs.json"), next); e != nil {
					return b, e
				}
				a.bugs = next
				return b, nil
			}
		}
		return b, errors.New("ID bug tidak ditemukan")
	}
	sum := sha256.Sum256([]byte(b.Page + "|" + b.Title))
	b.Fingerprint = hex.EncodeToString(sum[:])
	if b.Automatic {
		for i, old := range a.bugs {
			if old.Automatic && old.Fingerprint == b.Fingerprint && old.Status != "Selesai" {
				next := append([]Bug(nil), a.bugs...)
				next[i].Count++
				next[i].Updated = now
				if e := writeJSON(filepath.Join(a.dir, "bugs.json"), next); e != nil {
					return b, e
				}
				a.bugs = next
				return next[i], nil
			}
		}
	}
	if len(a.bugs) >= 10000 {
		return b, errors.New("Log bug mencapai 10000; ekspor lalu arsipkan sebelum menambah")
	}
	b.ID = "HT-" + time.Now().UTC().Format("20060102") + "-" + randomToken()[:8]
	b.Created = now
	b.Count = 1
	next := append(append([]Bug(nil), a.bugs...), b)
	if e := writeJSON(filepath.Join(a.dir, "bugs.json"), next); e != nil {
		return b, e
	}
	a.bugs = next
	return b, nil
}
func lanIPs() []string {
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, x := range addrs {
		ip, _, e := net.ParseCIDR(x.String())
		if e == nil && !ip.IsLoopback() && ip.To4() != nil && ip.IsPrivate() {
			out = append(out, ip.String())
		}
	}
	sort.Strings(out)
	return out
}
func (a *App) setLAN(enabled bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cloud {
		return errors.New("Mode LAN hanya tersedia pada EXE lokal")
	}
	if !enabled {
		if a.lan != nil {
			a.lan.Close()
			a.lan = nil
			a.lanPort = 0
		}
		return nil
	}
	if a.lan != nil {
		return nil
	}
	ln, e := net.Listen("tcp", ":0")
	if e != nil {
		return e
	}
	a.lanPort = ln.Addr().(*net.TCPAddr).Port
	a.lan = &http.Server{Handler: a.handler(true), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 90 * time.Second, WriteTimeout: 90 * time.Second, IdleTimeout: 60 * time.Second}
	srv := a.lan
	go srv.Serve(ln)
	return nil
}
func (a *App) handler(lan bool) http.Handler {
	static, _ := fs.Sub(assets, "web")
	files := http.FileServer(http.FS(static))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob: https://*.tile.openstreetmap.org; connect-src 'self' https://*.tile.openstreetmap.org; object-src 'none'; base-uri 'self'; frame-ancestors 'none'")
		if !a.cloud && !lan {
			host, _, e := net.SplitHostPort(r.Host)
			if e != nil || (host != "127.0.0.1" && host != "localhost" && host != "[::1]" && host != "::1") {
				http.Error(w, "Alamat host lokal ditolak", 403)
				return
			}
		}
		if lan {
			if q := r.URL.Query().Get("share"); equal(q, a.lanToken) {
				http.SetCookie(w, &http.Cookie{Name: "healthtara_view", Value: a.lanToken, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
			c, e := r.Cookie("healthtara_view")
			if e != nil || !equal(c.Value, a.lanToken) {
				http.Error(w, "Buka tautan berbagi lengkap dari menu Online dan pengaturan Healthtara.", 403)
				return
			}
		}
		path := r.URL.Path
		if path == "/api/ping" {
			jsonReply(w, map[string]any{"app": appID, "version": version})
			return
		}
		if path == "/api/meta" {
			a.mu.RLock()
			urls := []string{}
			if a.lan != nil {
				for _, ip := range lanIPs() {
					urls = append(urls, fmt.Sprintf("http://%s:%d/?share=%s", ip, a.lanPort, a.lanToken))
				}
			}
			cfg := a.config
			enabled := a.lan != nil
			port := a.port
			a.mu.RUnlock()
			isAdmin := !lan && !a.cloud && loopback(r) || a.admin(r, lan)
			m := map[string]any{"version": version, "cloud": a.cloud, "lanClient": lan, "isAdmin": isAdmin, "lanEnabled": enabled, "port": port, "publicURL": cfg.PublicURL, "lanURLs": urls, "adminLoginEnabled": a.cloud && a.adminKey != ""}
			if isAdmin {
				m["storagePath"] = a.dir
				if !a.cloud {
					m["token"] = a.token
				}
			} else {
				delete(m, "lanURLs")
			}
			jsonReply(w, m)
			return
		}
		if path == "/api/login" && r.Method == "POST" {
			if lan || !a.cloud || a.adminKey == "" {
				http.Error(w, "Login admin tidak tersedia", 403)
				return
			}
			var v struct {
				Key string `json:"key"`
			}
			if !decode(w, r, 4096, &v) {
				return
			}
			host, _, _ := net.SplitHostPort(r.RemoteAddr)
			a.mu.Lock()
			recent := []time.Time{}
			for _, t := range a.loginAttempts[host] {
				if time.Since(t) < time.Minute {
					recent = append(recent, t)
				}
			}
			if len(recent) >= 6 {
				a.mu.Unlock()
				http.Error(w, "Terlalu banyak percobaan; coba satu menit lagi", 429)
				return
			}
			a.loginAttempts[host] = append(recent, time.Now())
			if !equal(v.Key, a.adminKey) {
				a.mu.Unlock()
				http.Error(w, "Kunci admin tidak cocok", 403)
				return
			}
			t := randomToken()
			a.sessions[t] = time.Now().Add(8 * time.Hour)
			a.mu.Unlock()
			jsonReply(w, map[string]string{"token": t})
			return
		}
		if path == "/api/core" && r.Method == "GET" {
			a.serveJSONData(w, r, a.coreBytes(), false)
			return
		}
		if path == "/api/reference" && r.Method == "GET" {
			a.mu.RLock()
			var ref map[string]json.RawMessage
			if a.current != nil {
				ref = a.current.Reference
			}
			a.mu.RUnlock()
			if len(ref) > 0 {
				b, _ := json.Marshal(ref)
				a.serveJSONData(w, r, b, false)
			} else {
				b, e := assets.ReadFile("web/data/reference.json.gz")
				if e != nil {
					http.Error(w, "Kamus tidak tersedia", 500)
					return
				}
				a.serveJSONData(w, r, b, true)
			}
			return
		}
		if strings.HasPrefix(path, "/api/hospital/") && r.Method == "GET" {
			id := strings.TrimPrefix(path, "/api/hospital/")
			if !codePattern.MatchString(id) {
				http.NotFound(w, r)
				return
			}
			a.mu.RLock()
			active := a.ids[id]
			var data json.RawMessage
			if a.current != nil {
				data = a.current.Details[id]
			}
			a.mu.RUnlock()
			if !active {
				http.NotFound(w, r)
				return
			}
			if data != nil {
				a.serveJSONData(w, r, data, false)
			} else {
				b, e := a.embeddedDetail(id)
				if e != nil {
					http.NotFound(w, r)
					return
				}
				a.serveJSONData(w, r, b, true)
			}
			return
		}
		if path == "/api/import" && r.Method == "POST" {
			if !a.needAdmin(w, r, lan) {
				return
			}
			var p ImportPackage
			if !decode(w, r, importLimit, &p) {
				return
			}
			if e := validateImport(&p); e != nil {
				http.Error(w, e.Error(), 400)
				return
			}
			if e := a.saveImport(&p); e != nil {
				http.Error(w, e.Error(), 500)
				return
			}
			log.Printf("Import %s: %d hospitals", p.Origin, len(p.Hospitals))
			jsonReply(w, map[string]any{"ok": true, "rows": len(p.Hospitals)})
			return
		}
		if path == "/api/backups" && r.Method == "GET" {
			if !a.needAdmin(w, r, lan) {
				return
			}
			list := []map[string]any{{"name": "bundled", "label": "Data bawaan 12 Oktober 2026"}}
			entries, _ := os.ReadDir(filepath.Join(a.dir, "backups"))
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), "_dataset.json.gz") {
					i, _ := e.Info()
					list = append(list, map[string]any{"name": e.Name(), "bytes": i.Size(), "time": i.ModTime().UTC().Format(time.RFC3339)})
				}
			}
			jsonReply(w, list)
			return
		}
		if path == "/api/restore" && r.Method == "POST" {
			if !a.needAdmin(w, r, lan) {
				return
			}
			var v struct {
				Name string `json:"name"`
			}
			if !decode(w, r, 2048, &v) {
				return
			}
			if v.Name == "bundled" {
				a.mu.Lock()
				e := a.backupCurrent()
				if e == nil {
					e = os.Remove(filepath.Join(a.dir, "dataset.json.gz"))
					if os.IsNotExist(e) {
						e = nil
					}
				}
				if e == nil {
					a.current = nil
					a.rebuildIDs()
				}
				a.mu.Unlock()
				if e != nil {
					http.Error(w, e.Error(), 500)
					return
				}
			} else {
				if filepath.Base(v.Name) != v.Name || !strings.HasSuffix(v.Name, "_dataset.json.gz") {
					http.Error(w, "Nama backup ditolak", 400)
					return
				}
				p, e := readImport(filepath.Join(a.dir, "backups", v.Name))
				if e != nil {
					http.Error(w, e.Error(), 400)
					return
				}
				if e = a.saveImport(p); e != nil {
					http.Error(w, e.Error(), 500)
					return
				}
			}
			jsonReply(w, map[string]bool{"ok": true})
			return
		}
		if path == "/api/config" && r.Method == "POST" {
			if !a.needAdmin(w, r, lan) {
				return
			}
			var c Config
			if !decode(w, r, 4096, &c) {
				return
			}
			if c.PublicURL != "" {
				u, e := url.Parse(c.PublicURL)
				if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
					http.Error(w, "Alamat publik harus URL HTTPS tanpa kredensial", 400)
					return
				}
			}
			a.mu.Lock()
			e := writeJSON(filepath.Join(a.dir, "config.json"), c)
			if e == nil {
				a.config = c
			}
			a.mu.Unlock()
			if e != nil {
				http.Error(w, e.Error(), 500)
				return
			}
			jsonReply(w, map[string]bool{"ok": true})
			return
		}
		if path == "/api/lan" && r.Method == "POST" {
			if !a.needAdmin(w, r, lan) {
				return
			}
			var v struct {
				Enabled bool `json:"enabled"`
			}
			if !decode(w, r, 1024, &v) {
				return
			}
			if e := a.setLAN(v.Enabled); e != nil {
				http.Error(w, e.Error(), 400)
				return
			}
			jsonReply(w, map[string]bool{"ok": true})
			return
		}
		if path == "/api/bugs" {
			if !a.needAdmin(w, r, lan) {
				return
			}
			if r.Method == "GET" {
				a.mu.RLock()
				jsonReply(w, a.bugs)
				a.mu.RUnlock()
				return
			}
			if r.Method == "POST" {
				var b Bug
				if !decode(w, r, 18000, &b) {
					return
				}
				saved, e := a.saveBug(b)
				if e != nil {
					http.Error(w, e.Error(), 400)
					return
				}
				jsonReply(w, saved)
				return
			}
			http.Error(w, "Metode tidak didukung", 405)
			return
		}
		if path == "/api/diagnostics" && r.Method == "GET" {
			if !a.needAdmin(w, r, lan) {
				return
			}
			a.mu.RLock()
			v := map[string]any{"app": appID, "version": version, "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH, "cloud": a.cloud, "bugs": a.bugs, "generated": time.Now().UTC().Format(time.RFC3339), "hasImport": a.current != nil}
			a.mu.RUnlock()
			w.Header().Set("Content-Disposition", "attachment; filename=Healthtara_Diagnostik.json")
			jsonReply(w, v)
			return
		}
		if path == "/api/backup" && r.Method == "GET" {
			if !a.needAdmin(w, r, lan) {
				return
			}
			var buf bytes.Buffer
			z := zip.NewWriter(&buf)
			a.mu.RLock()
			for _, n := range []string{"dataset.json.gz", "bugs.json", "config.json"} {
				if b, e := os.ReadFile(filepath.Join(a.dir, n)); e == nil {
					f, _ := z.Create(n)
					f.Write(b)
				}
			}
			a.mu.RUnlock()
			f, _ := z.Create("PETUNJUK.txt")
			f.Write([]byte("Arsip keadaan lokal. Untuk memulihkan, tutup Healthtara; salin isi ke folder data pengguna yang ditampilkan pada Pengaturan; buka lagi EXE. Tidak berisi token atau kunci admin cloud. Simpan cadangan lama sebelum mengganti."))
			z.Close()
			w.Header().Set("Content-Type", "application/zip")
			w.Header().Set("Content-Disposition", "attachment; filename=Healthtara_Backup.zip")
			w.Write(buf.Bytes())
			return
		}
		if path == "/api/quit" && r.Method == "POST" {
			if !a.needAdmin(w, r, lan) {
				return
			}
			if a.cloud {
				http.Error(w, "Kelola server melalui penyedia hosting", 403)
				return
			}
			jsonReply(w, map[string]bool{"ok": true})
			go func() {
				time.Sleep(250 * time.Millisecond)
				select {
				case <-a.stop:
				default:
					close(a.stop)
				}
			}()
			return
		}
		if strings.HasPrefix(path, "/downloads/") {
			allowed := map[string]string{"tutorial.docx": "docs/Tutorial_Healthtara_v1.docx", "roadmap.docx": "docs/Roadmap_Pengembangan_Healthtara_v1.docx", "rekap.xlsx": "data/Healthcare_Intelligence_Rekap_Fasilitas_2026-10-12.xlsx"}
			rel, ok := allowed[strings.TrimPrefix(path, "/downloads/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			p := filepath.Join(a.root, filepath.FromSlash(rel))
			if _, e := os.Stat(p); e != nil {
				http.Error(w, "Dokumen ada pada folder docs/data di paket lengkap; letakkan di samping EXE.", 404)
				return
			}
			w.Header().Set("Content-Disposition", "attachment; filename="+filepath.Base(p))
			http.ServeFile(w, r, p)
			return
		}
		if strings.HasPrefix(path, "/api/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			http.Error(w, "Metode tidak didukung", 405)
			return
		}
		if strings.HasPrefix(path, "/data/details") || strings.HasSuffix(path, "hospital_details.zip") || strings.HasSuffix(path, "core.json.gz") || strings.HasSuffix(path, "reference.json.gz") {
			http.NotFound(w, r)
			return
		}
		files.ServeHTTP(w, r)
	})
}
func openBrowser(u string) error {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		c = exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", u)
	case "darwin":
		c = exec.Command("open", u)
	default:
		c = exec.Command("xdg-open", u)
	}
	return c.Start()
}

type RunState struct {
	Port    int    `json:"port"`
	Version string `json:"version"`
}

func main() {
	cloud := flag.Bool("cloud", os.Getenv("HEALTHTARA_MODE") == "cloud", "Serve on a hosting platform")
	noBrowser := flag.Bool("no-browser", false, "Do not open browser")
	dir := flag.String("data-dir", defaultDir(), "User data directory")
	port := flag.Int("port", 8564, "Preferred local port")
	lan := flag.Bool("lan", false, "Enable read-only LAN sharing")
	online := flag.Bool("online", false, "Open saved public website")
	ver := flag.Bool("version", false, "Show version")
	flag.Parse()
	if *ver {
		fmt.Println("Healthtara", version, runtime.GOOS, runtime.GOARCH)
		return
	}
	exe, _ := os.Executable()
	root := filepath.Dir(exe)
	if r := os.Getenv("HEALTHTARA_PACKAGE_ROOT"); r != "" {
		root = r
	}
	os.MkdirAll(*dir, 0700)
	lp := filepath.Join(*dir, "healthtara.log")
	if st, e := os.Stat(lp); e == nil && st.Size() > 2<<20 {
		os.Rename(lp, lp+".1")
	}
	lf, e := os.OpenFile(lp, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e == nil {
		defer lf.Close()
		log.SetOutput(lf)
	}
	a, e := newApp(*dir, root, *cloud)
	if e != nil {
		notify("Healthtara tidak dapat dimulai", e.Error()+"\nLog: "+lp)
		return
	}
	if *online && a.config.PublicURL != "" {
		if e = openBrowser(a.config.PublicURL); e != nil {
			notify("Browser tidak dapat dibuka", a.config.PublicURL)
		}
		return
	}
	runPath := filepath.Join(*dir, "running.json")
	if !*cloud {
		if b, e := os.ReadFile(runPath); e == nil {
			var prev RunState
			json.Unmarshal(b, &prev)
			client := http.Client{Timeout: 700 * time.Millisecond}
			u := fmt.Sprintf("http://127.0.0.1:%d", prev.Port)
			if resp, e := client.Get(u + "/api/ping"); e == nil {
				var m map[string]string
				json.NewDecoder(resp.Body).Decode(&m)
				resp.Body.Close()
				if m["app"] == appID {
					if m["version"] != version {
						notify("Healthtara sedang berjalan", "Tutup versi sebelumnya melalui menu Pengaturan sebelum membuka versi baru.")
						return
					}
					if !*noBrowser {
						if *online || *lan {
							u += "/#settings"
						}
						openBrowser(u)
					}
					return
				}
			}
		}
	}
	bind := fmt.Sprintf("127.0.0.1:%d", *port)
	if *cloud {
		p := os.Getenv("PORT")
		if p == "" {
			p = "8080"
		}
		bind = "0.0.0.0:" + p
	}
	ln, e := net.Listen("tcp", bind)
	if e != nil && !*cloud {
		ln, e = net.Listen("tcp", "127.0.0.1:0")
	}
	if e != nil {
		notify("Port tidak tersedia", e.Error())
		return
	}
	a.port = ln.Addr().(*net.TCPAddr).Port
	server := &http.Server{Handler: a.handler(false), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 120 * time.Second, WriteTimeout: 120 * time.Second, IdleTimeout: 60 * time.Second}
	if !*cloud {
		writeJSON(runPath, RunState{Port: a.port, Version: version})
	}
	if *lan {
		if e = a.setLAN(true); e != nil {
			log.Printf("LAN: %v", e)
		}
	}
	go func() {
		if e := server.Serve(ln); e != nil && e != http.ErrServerClosed {
			log.Printf("Serve error: %v", e)
			close(a.stop)
		}
	}()
	u := fmt.Sprintf("http://127.0.0.1:%d/", a.port)
	log.Printf("Started Healthtara %s at %s cloud=%v", version, u, *cloud)
	if !*cloud && !*noBrowser {
		if *online || *lan {
			u += "#settings"
		}
		if e = openBrowser(u); e != nil {
			notify("Buka Healthtara di browser", u)
		}
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	select {
	case <-a.stop:
	case <-signals:
	}
	a.setLAN(false)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	server.Shutdown(ctx)
	if !*cloud {
		os.Remove(runPath)
	}
	log.Printf("Stopped Healthtara")
}
