// Package demo menyediakan server latihan yang meniru perilaku aplikasi Laravel
// (API token, form login dengan CSRF + session, validasi 422, error 5xx) untuk mencoba serbu.
package demo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	mrand "math/rand"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type produk struct {
	ID    int    `json:"id"`
	Nama  string `json:"nama"`
	Harga int    `json:"harga"`
	Stok  int    `json:"stok"`
}

var katalog = func() []produk {
	nama := []string{"Kopi Susu", "Teh Manis", "Nasi Goreng", "Mie Ayam", "Bakso Urat", "Sate Ayam", "Es Jeruk",
		"Roti Bakar", "Pisang Goreng", "Martabak Manis", "Soto Ayam", "Gado-gado", "Rendang", "Pecel Lele",
		"Es Cendol", "Nasi Uduk", "Ayam Geprek", "Siomay", "Batagor", "Es Teler"}
	out := make([]produk, len(nama))
	for i, n := range nama {
		out[i] = produk{ID: i + 1, Nama: n, Harga: 5000 + (i*3700)%30000, Stok: 10 + (i*7)%50}
	}
	return out
}()

type session struct {
	csrf  string
	email string
}

type server struct {
	workers  chan struct{}
	inflight atomic.Int64
	served   atomic.Int64

	mu       sync.Mutex
	sessions map[string]*session
	tokens   map[string]string
	orders   atomic.Int64
}

func token(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// work mensimulasikan waktu proses; antre bila semua worker sibuk (seperti PHP-FPM).
func (s *server) work(base, jitter time.Duration) {
	s.workers <- struct{}{}
	defer func() { <-s.workers }()
	n := s.inflight.Add(1)
	defer s.inflight.Add(-1)
	d := base + time.Duration(mrand.Int63n(int64(jitter)+1)) + time.Duration(n)*200*time.Microsecond
	time.Sleep(d)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *server) sess(w http.ResponseWriter, r *http.Request) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, err := r.Cookie("serbu_session"); err == nil {
		if ss, ok := s.sessions[c.Value]; ok {
			return ss
		}
	}
	id := token(16)
	ss := &session{csrf: token(20)}
	s.sessions[id] = ss
	http.SetCookie(w, &http.Cookie{Name: "serbu_session", Value: id, Path: "/", HttpOnly: true})
	return ss
}

func (s *server) bearer(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	email, ok := s.tokens[strings.TrimPrefix(h, "Bearer ")]
	return email, ok
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, indexHTML)
	})
	mux.HandleFunc("GET /api/produk", func(w http.ResponseWriter, r *http.Request) {
		s.work(10*time.Millisecond, 15*time.Millisecond)
		q := strings.ToLower(r.URL.Query().Get("q"))
		data := make([]produk, 0, len(katalog))
		for _, p := range katalog {
			if q == "" || strings.Contains(strings.ToLower(p.Nama), q) {
				data = append(data, p)
			}
		}
		writeJSON(w, 200, map[string]any{"data": data, "total": len(data)})
	})
	mux.HandleFunc("GET /api/produk/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.work(5*time.Millisecond, 8*time.Millisecond)
		id, _ := strconv.Atoi(r.PathValue("id"))
		if id < 1 || id > len(katalog) {
			writeJSON(w, 404, map[string]string{"message": "Produk tidak ditemukan."})
			return
		}
		writeJSON(w, 200, map[string]any{"data": katalog[id-1]})
	})
	mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, r *http.Request) {
		s.work(40*time.Millisecond, 30*time.Millisecond)
		var in struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			_ = json.NewDecoder(r.Body).Decode(&in)
		} else {
			in.Email, in.Password = r.FormValue("email"), r.FormValue("password")
		}
		errs := map[string][]string{}
		if in.Email == "" {
			errs["email"] = []string{"Kolom email wajib diisi."}
		}
		if in.Password == "" {
			errs["password"] = []string{"Kolom password wajib diisi."}
		}
		if len(errs) > 0 {
			writeJSON(w, 422, map[string]any{"message": "Data yang diberikan tidak valid.", "errors": errs})
			return
		}
		if in.Password != "rahasia" {
			writeJSON(w, 401, map[string]string{"message": "Email atau password salah."})
			return
		}
		t := token(20)
		s.mu.Lock()
		s.tokens[t] = in.Email
		s.mu.Unlock()
		writeJSON(w, 200, map[string]any{"token": t, "user": map[string]string{"email": in.Email, "nama": strings.Split(in.Email, "@")[0]}})
	})
	mux.HandleFunc("GET /api/profil", func(w http.ResponseWriter, r *http.Request) {
		s.work(5*time.Millisecond, 5*time.Millisecond)
		email, ok := s.bearer(r)
		if !ok {
			writeJSON(w, 401, map[string]string{"message": "Unauthenticated."})
			return
		}
		writeJSON(w, 200, map[string]any{"data": map[string]string{"email": email}})
	})
	mux.HandleFunc("POST /api/pesanan", func(w http.ResponseWriter, r *http.Request) {
		s.work(30*time.Millisecond, 40*time.Millisecond)
		if _, ok := s.bearer(r); !ok {
			writeJSON(w, 401, map[string]string{"message": "Unauthenticated."})
			return
		}
		var in struct {
			ProdukID int `json:"produk_id"`
			Jumlah   int `json:"jumlah"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.ProdukID < 1 || in.ProdukID > len(katalog) || in.Jumlah < 1 {
			writeJSON(w, 422, map[string]any{"message": "Data pesanan tidak valid.", "errors": map[string][]string{"produk_id": {"Produk tidak ada."}}})
			return
		}
		writeJSON(w, 201, map[string]any{"data": map[string]any{"id": s.orders.Add(1), "produk_id": in.ProdukID, "jumlah": in.Jumlah, "total": katalog[in.ProdukID-1].Harga * in.Jumlah}})
	})
	mux.HandleFunc("GET /lambat", func(w http.ResponseWriter, r *http.Request) {
		d, _ := strconv.Atoi(r.URL.Query().Get("ms"))
		if d <= 0 {
			d = 500
		}
		if d > 30000 {
			d = 30000
		}
		select {
		case <-time.After(time.Duration(d) * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		fmt.Fprintf(w, "selesai setelah %dms\n", d)
	})
	mux.HandleFunc("GET /kadang-error", func(w http.ResponseWriter, r *http.Request) {
		s.work(5*time.Millisecond, 10*time.Millisecond)
		p, err := strconv.Atoi(r.URL.Query().Get("persen"))
		if err != nil {
			p = 10
		}
		if mrand.Intn(100) < p {
			writeJSON(w, 500, map[string]string{"message": "Server Error"})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		s.work(8*time.Millisecond, 8*time.Millisecond)
		ss := s.sess(w, r)
		http.SetCookie(w, &http.Cookie{Name: "XSRF-TOKEN", Value: ss.csrf, Path: "/"})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, loginHTML, ss.csrf, ss.csrf)
	})
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		s.work(40*time.Millisecond, 30*time.Millisecond)
		ss := s.sess(w, r)
		if r.FormValue("_token") == "" || r.FormValue("_token") != ss.csrf {
			w.WriteHeader(419)
			fmt.Fprint(w, "419 | Page Expired (token CSRF tidak cocok)")
			return
		}
		if r.FormValue("password") != "rahasia" || r.FormValue("email") == "" {
			http.Redirect(w, r, "/login?gagal=1", http.StatusFound)
			return
		}
		s.mu.Lock()
		ss.email = r.FormValue("email")
		s.mu.Unlock()
		http.Redirect(w, r, "/dashboard", http.StatusFound)
	})
	mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, r *http.Request) {
		s.work(15*time.Millisecond, 15*time.Millisecond)
		ss := s.sess(w, r)
		s.mu.Lock()
		email := ss.email
		s.mu.Unlock()
		if email == "" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<!doctype html><title>Dashboard</title><h1>Selamat datang, %s</h1>", html.EscapeString(email))
	})
	mux.HandleFunc("POST /upload", func(w http.ResponseWriter, r *http.Request) {
		s.work(20*time.Millisecond, 20*time.Millisecond)
		f, hdr, err := r.FormFile("berkas")
		if err != nil {
			writeJSON(w, 422, map[string]string{"message": "Field 'berkas' wajib berisi file."})
			return
		}
		defer f.Close()
		n, _ := io.Copy(io.Discard, f)
		writeJSON(w, 200, map[string]any{"nama": hdr.Filename, "ukuran": n, "tipe": hdr.Header.Get("Content-Type"), "keterangan": r.FormValue("keterangan")})
	})
	mux.HandleFunc("GET /redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/api/produk", http.StatusFound)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.served.Add(1)
		mux.ServeHTTP(w, r)
	})
}

// Run menjalankan server demo sampai Ctrl+C.
func Run(port, workers int) error {
	if workers < 1 {
		workers = 1
	}
	s := &server{workers: make(chan struct{}, workers), sessions: map[string]*session{}, tokens: map[string]string{}}
	srv := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: s.routes(), ReadHeaderTimeout: 10 * time.Second}
	fmt.Printf(banner, port, workers)
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	var last int64
	for {
		select {
		case err := <-errCh:
			return err
		case <-sig:
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
			fmt.Printf("\n  server demo berhenti, total %d request dilayani\n", s.served.Load())
			return nil
		case <-tick.C:
			if n := s.served.Load(); n != last {
				fmt.Printf("  · %d request dilayani (+%d/5s), sedang diproses: %d\n", n, n-last, s.inflight.Load())
				last = n
			}
		}
	}
}

const banner = `
  serbu demo — server latihan berjalan di http://localhost:%d
  Simulasi %d worker (seperti PHP-FPM): request ekstra akan antre. Ctrl+C untuk berhenti.

  GET  /api/produk           daftar produk (JSON), ?q= untuk mencari
  GET  /api/produk/{id}      detail produk, 404 bila tidak ada
  POST /api/login            {email, password} → {token}  (password: rahasia)
  GET  /api/profil           butuh header Authorization: Bearer <token>
  POST /api/pesanan          {produk_id, jumlah}, butuh token → 201
  GET  /login, POST /login   form login dengan CSRF + cookie session (gaya Laravel, 419 bila token salah)
  GET  /dashboard            butuh login via form
  POST /upload               multipart, field "berkas"
  GET  /lambat?ms=500        respons lambat
  GET  /kadang-error?persen=10   sebagian request dibalas 500

`

const indexHTML = `<!doctype html><meta charset="utf-8"><title>serbu demo</title>
<h1>serbu demo</h1><p>Server latihan untuk mencoba <code>serbu run</code>. Lihat daftar endpoint di terminal.</p>`

const loginHTML = `<!doctype html><html><head><meta charset="utf-8"><meta name="csrf-token" content="%s"><title>Login</title></head>
<body><form method="POST" action="/login">
<input type="hidden" name="_token" value="%s">
<input type="email" name="email"><input type="password" name="password"><button>Masuk</button>
</form></body></html>`
