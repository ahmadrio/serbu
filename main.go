// serbu — alat uji beban (load test) aplikasi web bergaya k6, ditulis dengan Go.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"serbu/internal/demo"
	"serbu/internal/engine"
)

const version = "0.1.0"

const helpText = `serbu v%s — uji beban (load test) aplikasi web, gaya k6

Pemakaian:
  serbu run [flag] <script.js|script.ts>   jalankan tes
  serbu new [nama-file] [--force]          buat contoh script (default: tes.js)
  serbu demo [--port 8787] [--workers 50]  server latihan untuk mencoba serbu
  serbu version                            tampilkan versi

Flag untuk run (menimpa options di script):
  -u, --vus N                  jumlah virtual user (VU)
  -d, --duration DURASI        lama tes, mis. 30s, 5m, 1h
  -i, --iterations N           total iterasi (dibagi ke semua VU)
  -s, --stage DURASI:TARGET    tahapan VU, bisa diulang: -s 30s:20 -s 1m:20 -s 10s:0
  -e, --env NAMA=NILAI         variabel untuk __ENV, bisa diulang
      --summary-export FILE    simpan ringkasan hasil sebagai JSON
      --out json=FILE          simpan semua titik metrik (NDJSON, format k6)
      --summary-trend-stats S  statistik trend, mis. "avg,med,p(95),p(99),max"
      --http-debug[=full]      tampilkan request/response (full = beserta body)
      --insecure-skip-tls-verify  abaikan error sertifikat TLS (server uji)
      --user-agent TEKS        ganti header User-Agent
      --no-thresholds          lewati evaluasi threshold
      --no-summary             jangan tampilkan ringkasan di akhir
      --no-color               matikan warna
  -q, --quiet                  sembunyikan progress

Exit code: 0 lulus · 99 threshold gagal · 104 konfigurasi salah
           105 dihentikan (Ctrl+C) · 107 error script · 108 test.abort()

Contoh:
  serbu new tes.js && serbu run tes.js
  serbu run -u 50 -d 1m -e BASE_URL=https://staging.contoh.com tes.js
  serbu run --summary-export hasil.json tes.ts
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Printf(helpText, version)
		return 0
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:])
	case "new", "init":
		return cmdNew(args[1:])
	case "demo":
		return cmdDemo(args[1:])
	case "version", "--version", "-v":
		fmt.Println("serbu v" + version)
		return 0
	case "help", "-h", "--help":
		fmt.Printf(helpText, version)
		return 0
	}
	if strings.HasSuffix(args[0], ".js") || strings.HasSuffix(args[0], ".ts") {
		return cmdRun(args)
	}
	fmt.Fprintf(os.Stderr, "Perintah %q tidak dikenal. Lihat: serbu help\n", args[0])
	return 2
}

type optInt struct {
	v   int64
	set bool
}

func (o *optInt) String() string { return strconv.FormatInt(o.v, 10) }
func (o *optInt) Set(s string) error {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return errors.New("harus angka bulat positif")
	}
	o.v, o.set = n, true
	return nil
}

type optDur struct {
	v   time.Duration
	set bool
}

func (o *optDur) String() string { return o.v.String() }
func (o *optDur) Set(s string) error {
	d, err := engine.ParseDuration(s)
	if err != nil {
		return err
	}
	o.v, o.set = d, true
	return nil
}

type stageList []engine.StagePlan

func (s *stageList) String() string { return "" }
func (s *stageList) Set(v string) error {
	d, t, ok := strings.Cut(v, ":")
	if !ok {
		return errors.New("format stage harus DURASI:TARGET, mis. 30s:20")
	}
	dur, err := engine.ParseDuration(d)
	if err != nil {
		return err
	}
	n, err := strconv.ParseFloat(t, 64)
	if err != nil || n < 0 {
		return errors.New("target stage harus angka ≥ 0")
	}
	*s = append(*s, engine.StagePlan{Duration: dur, Target: n})
	return nil
}

type envMap map[string]string

func (e envMap) String() string { return "" }
func (e envMap) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || k == "" {
		return errors.New("format harus NAMA=NILAI")
	}
	e[k] = val
	return nil
}

// optStr untuk flag yang boleh tanpa nilai (--http-debug atau --http-debug=full).
type optStr struct{ v string }

func (o *optStr) String() string   { return o.v }
func (o *optStr) IsBoolFlag() bool { return true }
func (o *optStr) Set(s string) error {
	switch s {
	case "true", "":
		o.v = "headers"
	case "false":
		o.v = ""
	case "full", "headers":
		o.v = s
	default:
		return errors.New("nilai harus kosong atau 'full'")
	}
	return nil
}

// parseInterspersed memungkinkan flag ditulis sebelum atau sesudah nama script.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		vus, iters                                   optInt
		dur                                          optDur
		stages                                       stageList
		env                                          = envMap{}
		httpDebug                                    optStr
		summaryExport, out, trendStats, userAgent    string
		quiet, noColor, noThresholds, noSummary, tls bool
	)
	for _, n := range []string{"u", "vus"} {
		fs.Var(&vus, n, "")
	}
	for _, n := range []string{"i", "iterations"} {
		fs.Var(&iters, n, "")
	}
	for _, n := range []string{"d", "duration"} {
		fs.Var(&dur, n, "")
	}
	for _, n := range []string{"s", "stage"} {
		fs.Var(&stages, n, "")
	}
	for _, n := range []string{"e", "env"} {
		fs.Var(env, n, "")
	}
	for _, n := range []string{"q", "quiet"} {
		fs.BoolVar(&quiet, n, false, "")
	}
	fs.Var(&httpDebug, "http-debug", "")
	fs.StringVar(&summaryExport, "summary-export", "", "")
	fs.StringVar(&out, "out", "", "")
	fs.StringVar(&out, "o", "", "")
	fs.StringVar(&trendStats, "summary-trend-stats", "", "")
	fs.StringVar(&userAgent, "user-agent", "\x00", "")
	fs.BoolVar(&noColor, "no-color", false, "")
	fs.BoolVar(&noThresholds, "no-thresholds", false, "")
	fs.BoolVar(&noSummary, "no-summary", false, "")
	fs.BoolVar(&tls, "insecure-skip-tls-verify", false, "")

	pos, err := parseInterspersed(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Printf(helpText, version)
			return 0
		}
		fmt.Fprintf(os.Stderr, "✗ %v\n  Lihat daftar flag: serbu help\n", translateFlagErr(err))
		return engine.ExitConfig
	}
	if len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "✗ Sebutkan satu file script. Contoh: serbu run tes.js")
		return engine.ExitConfig
	}

	cfg := engine.Config{
		Script: pos[0], Version: version, Env: env, SummaryExport: summaryExport,
		HTTPDebug: httpDebug.v, Quiet: quiet, NoColor: noColor, NoThresholds: noThresholds,
		NoSummary: noSummary, InsecureTLS: tls,
	}
	if vus.set {
		cfg.CLI.VUs = &vus.v
	}
	if iters.set {
		cfg.CLI.Iterations = &iters.v
	}
	if dur.set {
		cfg.CLI.Duration = &dur.v
	}
	cfg.CLI.Stages = stages
	if userAgent != "\x00" {
		cfg.UserAgent = &userAgent
	}
	if trendStats != "" {
		for _, s := range strings.Split(trendStats, ",") {
			if s = strings.TrimSpace(s); s != "" {
				cfg.TrendStats = append(cfg.TrendStats, s)
			}
		}
	}
	if out != "" {
		kind, path, ok := strings.Cut(out, "=")
		switch {
		case ok && kind == "json":
			cfg.JSONOut = path
		case !ok && (strings.HasSuffix(out, ".json") || strings.HasSuffix(out, ".ndjson")):
			cfg.JSONOut = out
		default:
			fmt.Fprintf(os.Stderr, "✗ --out %q tidak didukung. Gunakan --out json=FILE\n", out)
			return engine.ExitConfig
		}
	}
	return engine.Run(cfg)
}

func translateFlagErr(err error) string {
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "flag provided but not defined: "):
		return "flag tidak dikenal: " + strings.TrimPrefix(msg, "flag provided but not defined: ")
	case strings.HasPrefix(msg, "flag needs an argument: "):
		return "flag butuh nilai: " + strings.TrimPrefix(msg, "flag needs an argument: ")
	case strings.HasPrefix(msg, "invalid value "):
		// format asli: invalid value "abc" for flag -vus: alasan
		rest := strings.TrimPrefix(msg, "invalid value ")
		if val, after, ok := strings.Cut(rest, " for flag "); ok {
			flagName, reason, _ := strings.Cut(after, ": ")
			return fmt.Sprintf("nilai %s untuk flag %s tidak valid: %s", val, flagName, reason)
		}
		return "nilai flag tidak valid: " + rest
	case strings.HasPrefix(msg, "invalid boolean value "):
		return "flag ini tidak butuh nilai: " + strings.TrimPrefix(msg, "invalid boolean value ")
	}
	return msg
}

func cmdNew(args []string) int {
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var force bool
	fs.BoolVar(&force, "force", false, "")
	fs.BoolVar(&force, "f", false, "")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ %v\n", translateFlagErr(err))
		return 2
	}
	name := "tes.js"
	if len(pos) > 0 {
		name = pos[0]
	}
	if !strings.HasSuffix(name, ".js") && !strings.HasSuffix(name, ".ts") {
		name += ".js"
	}
	if _, err := os.Stat(name); err == nil && !force {
		fmt.Fprintf(os.Stderr, "✗ %s sudah ada. Pakai --force untuk menimpa.\n", name)
		return 2
	}
	tpl := templateJS
	if strings.HasSuffix(name, ".ts") {
		tpl = templateTS
	}
	if err := os.WriteFile(name, []byte(tpl), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "✗ Gagal membuat %s: %v\n", name, err)
		return 2
	}
	fmt.Printf("✓ %s dibuat.\n\n  Coba langsung dengan server latihan:\n    serbu demo            (di terminal lain)\n    serbu run %s\n\n  Atau arahkan ke aplikasimu:\n    serbu run -e BASE_URL=https://staging.contoh.com %s\n", name, name, name)
	return 0
}

func cmdDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	port := fs.Int("port", 8787, "")
	workers := fs.Int("workers", 50, "")
	if _, err := parseInterspersed(fs, args); err != nil {
		fmt.Fprintf(os.Stderr, "✗ %v\n", translateFlagErr(err))
		return 2
	}
	if err := demo.Run(*port, *workers); err != nil {
		fmt.Fprintf(os.Stderr, "✗ Server demo gagal: %v\n", err)
		return 1
	}
	return 0
}

const templateJS = `// Script uji beban serbu — sintaksnya sama dengan k6.
// Jalankan  : serbu run tes.js
// Ganti URL : serbu run -e BASE_URL=https://situs-kamu.com tes.js
// Belum punya target? Jalankan "serbu demo" di terminal lain.

import http from 'k6/http';
import { check, sleep } from 'k6';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8787';

export const options = {
  // Naik ke 10 VU dalam 10 detik, tahan 30 detik, lalu turun.
  stages: [
    { duration: '10s', target: 10 },
    { duration: '30s', target: 10 },
    { duration: '10s', target: 0 },
  ],
  // Kriteria lulus. Bila ada yang gagal, serbu keluar dengan kode 99.
  thresholds: {
    http_req_failed: ['rate<0.01'], // error di bawah 1%
    http_req_duration: ['p(95)<500'], // 95% request di bawah 500ms
  },
};

export default function () {
  const res = http.get(BASE_URL + '/api/produk');

  check(res, {
    'status 200': (r) => r.status === 200,
    'ada data produk': (r) => r.json('data.#') > 0,
  });

  sleep(1); // jeda antar aksi, seperti pengunjung sungguhan
}
`

const templateTS = `// Script uji beban serbu (TypeScript) — sintaksnya sama dengan k6.
// Jalankan  : serbu run tes.ts
// Ganti URL : serbu run -e BASE_URL=https://situs-kamu.com tes.ts
// Autocomplete di editor: npm i -D @types/k6

import http from 'k6/http';
import { check, sleep } from 'k6';

const BASE_URL: string = __ENV.BASE_URL || 'http://localhost:8787';

interface Produk {
  id: number;
  nama: string;
  harga: number;
}

export const options = {
  stages: [
    { duration: '10s', target: 10 },
    { duration: '30s', target: 10 },
    { duration: '10s', target: 0 },
  ],
  thresholds: {
    http_req_failed: ['rate<0.01'],
    http_req_duration: ['p(95)<500'],
  },
};

export default function (): void {
  const res = http.get(BASE_URL + '/api/produk');
  const produk = res.json('data') as unknown as Produk[];

  check(res, {
    'status 200': (r) => r.status === 200,
    'ada data produk': () => Array.isArray(produk) && produk.length > 0,
  });

  sleep(1);
}
`
