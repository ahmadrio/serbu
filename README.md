# serbu

Alat uji beban (*load test*) untuk aplikasi web, bergaya **k6**. Satu file binary tanpa dependensi, script ditulis dalam **JavaScript atau TypeScript** dengan API yang sama seperti k6, dan hasilnya berbahasa Indonesia.

```
serbu run tes.js
```

- Virtual user (VU), tahapan naik-turun, dan 6 executor k6 (termasuk *arrival rate*)
- Check, group, threshold lulus/gagal (exit code 99 untuk CI), metrik custom
- Rincian waktu tiap request: DNS/antre, koneksi TCP, TLS, kirim, menunggu server (TTFB), terima
- Ringkasan terminal + distribusi status HTTP, daftar error jaringan, dan catatan singkat
- Ekspor JSON ringkasan (`--summary-export`), titik metrik mentah (`--out json=`), atau laporan custom lewat `handleSummary()`
- Script k6 yang memakai API dasar bisa langsung dijalankan

> Uji hanya server milikmu sendiri atau yang sudah mengizinkan. Mulai dari VU kecil, idealnya ke staging.

## Instal

**Binary jadi** — ambil sesuai sistem: `serbu-darwin-arm64` (Mac M1/M2/M3/M4), `serbu-darwin-amd64` (Mac Intel), `serbu-linux-amd64`, `serbu-linux-arm64`, `serbu-windows-amd64.exe`.

```bash
chmod +x serbu-darwin-arm64
xattr -d com.apple.quarantine serbu-darwin-arm64   # macOS: izinkan binary hasil unduhan
sudo mv serbu-darwin-arm64 /usr/local/bin/serbu
serbu version
```

**Build sendiri** (Go 1.24+): `go build -o serbu .` atau `make build-all` untuk semua platform ke folder `dist/`.

## Mulai cepat

```bash
serbu demo          # terminal 1: server latihan di http://localhost:8787
serbu new tes.js    # terminal 2: buat contoh script (pakai .ts untuk TypeScript)
serbu run tes.js
```

Arahkan ke aplikasimu dan ubah beban dari CLI tanpa mengedit script:

```bash
serbu run -e BASE_URL=https://staging.contoh.com tes.js
serbu run -u 50 -d 2m tes.js                    # 50 VU selama 2 menit
serbu run -s 30s:20 -s 1m:20 -s 15s:0 tes.js    # naik ke 20 VU, tahan, turun
```

Server demo meniru perilaku aplikasi Laravel (token API, form login dengan CSRF + session dan status 419, validasi 422, error 5xx, 50 worker seperti PHP-FPM sehingga request ekstra antre). Login uji: email apa saja, password `rahasia`.

## Contoh script

| File | Isi |
|---|---|
| [`examples/01-dasar.js`](examples/01-dasar.js) | Beban konstan, check, tag `name` untuk URL dinamis, threshold per endpoint |
| [`examples/02-api-login.ts`](examples/02-api-login.ts) | TypeScript, login token sekali di `setup()`, `group()`, metrik custom, import file lokal |
| [`examples/03-form-csrf.js`](examples/03-form-csrf.js) | Login form web Laravel: ambil `_token` CSRF, kirim form, ikuti redirect, cookie session |
| [`examples/04-skenario.js`](examples/04-skenario.js) | Dua skenario paralel (laju tetap 20/detik + VU naik-turun), `abortOnFail`, `k6/execution` |
| [`examples/05-upload-batch.js`](examples/05-upload-batch.js) | Upload file multipart, `http.batch()` paralel, `expectedStatuses`, `handleSummary()` |

## Menulis script

Sintaksnya sama dengan k6, jadi dokumentasi dan contoh k6 bisa dipakai.

```js
import http from 'k6/http';
import { check, sleep } from 'k6';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8787';

export const options = {
  vus: 10,
  duration: '30s',
  thresholds: {
    http_req_failed: ['rate<0.01'],     // error < 1%
    http_req_duration: ['p(95)<500'],   // 95% request < 500ms
  },
};

export function setup() {            // opsional, sekali sebelum tes
  const res = http.post(`${BASE_URL}/api/login`, JSON.stringify({ email: 'uji@contoh.com', password: 'rahasia' }),
    { headers: { 'Content-Type': 'application/json' } });
  return { token: res.json('token') };
}

export default function (data) {     // dijalankan berulang oleh setiap VU
  const res = http.get(`${BASE_URL}/api/profil`, { headers: { Authorization: `Bearer ${data.token}` } });
  check(res, { 'status 200': (r) => r.status === 200 });
  sleep(1);
}
```

Kode di luar function (*init context*) dijalankan sekali per VU: tempat untuk `import`, `options`, `open()`, dan `new Trend(...)`. Import file lokal (`./lib/helper.ts`) dibundel otomatis.

**Autocomplete di VS Code:** `npm i -D @types/k6`. Karena import memakai jalur `k6/...`, tipe k6 langsung berlaku.

## Yang didukung

**Opsi:** `vus`, `duration`, `iterations`, `stages`, `scenarios`, `thresholds` (termasuk submetric seperti `http_req_duration{name:login}` dan objek `{threshold, abortOnFail, delayAbortEval}`), `userAgent`, `insecureSkipTLSVerify`, `maxRedirects`, `noCookiesReset`, `noConnectionReuse`, `noVUConnectionReuse`, `discardResponseBodies`, `batch`, `batchPerHost`, `hosts`, `setupTimeout`, `teardownTimeout`, `summaryTrendStats`, `tags`.

**Executor:** `constant-vus`, `ramping-vus`, `shared-iterations`, `per-vu-iterations`, `constant-arrival-rate`, `ramping-arrival-rate` — dengan `exec`, `startTime`, `gracefulStop`, `gracefulRampDown`, `env`, `tags`.

**Modul** (bisa diimport sebagai `k6/...` atau `serbu/...`):

| Modul | Isi |
|---|---|
| `k6` | `check`, `sleep`, `group`, `fail`, `randomSeed` |
| `k6/http` | `get`, `post`, `put`, `patch`, `del`, `head`, `options`, `request`, `batch`, `file`, `cookieJar`, `expectedStatuses`, `setResponseCallback`. Params: `headers`, `tags`, `timeout`, `redirects`, `responseType`, `cookies`, `responseCallback`. Response: `status`, `body`, `headers`, `cookies`, `timings`, `json(path)`, `url`, `error`, `error_code`, `request` |
| `k6/metrics` | `Counter`, `Gauge`, `Rate`, `Trend` |
| `k6/execution` | `vu`, `scenario` (termasuk `iterationInTest` untuk data unik), `instance`, `test.abort()` |
| `k6/encoding`, `k6/crypto`, `k6/data` | base64, hash/HMAC, `SharedArray` |
| `serbu/utils` | `findBetween`, `randomIntBetween`, `randomItem`, `randomString`, `uuidv4` |
| `serbu/summary` | `textSummary` |

Import `https://jslib.k6.io/k6-utils/...` dan `https://jslib.k6.io/k6-summary/...` otomatis memakai versi bawaan (tanpa unduh). Global: `__VU`, `__ITER`, `__ENV`, `open()`, `console`.

**Belum didukung:** `res.html()` (pakai `findBetween()` atau regex), WebSocket, gRPC, modul browser, import URL selain jslib di atas, `setTimeout`, output selain JSON.

## Membaca hasil

| Metrik | Artinya |
|---|---|
| `http_req_duration` | Waktu request = kirim + tunggu + terima. Pakai **p(95)**: 95% request lebih cepat dari angka ini |
| `http_req_waiting` | TTFB, waktu server memproses (PHP, query database). Biasanya penyebab utama lambat |
| `http_req_blocked` / `connecting` / `tls_handshaking` | Antre koneksi + DNS, buka TCP, jabat tangan TLS |
| `http_req_failed` | Persentase request gagal: tanpa respons, atau status di luar 200–399 (bisa diatur dengan `expectedStatuses`) |
| `iterations`, `vus` | Jumlah iterasi selesai, jumlah VU aktif |
| `data_sent` / `data_received` | Byte yang lewat jaringan |

Di bawahnya ada **RESPONS HTTP** (sebaran status dan error jaringan seperti koneksi ditolak/timeout/DNS) dan **CATATAN** (misalnya banyak 5xx atau 429, iterasi yang tidak sempat jalan).

## Output, CI, dan exit code

```bash
serbu run --summary-export hasil.json tes.js     # ringkasan JSON (format data handleSummary k6)
serbu run --out json=metrik.ndjson tes.js        # semua titik metrik, format sama dengan k6 --out json
serbu run --http-debug=full tes.js               # tampilkan request/response untuk menyusun script
```

| Exit | Arti |
|---|---|
| 0 | Selesai, semua threshold lulus |
| 99 | Ada threshold gagal — job CI otomatis merah |
| 104 | Konfigurasi/opsi salah |
| 105 | Dihentikan dengan Ctrl+C (teardown dan ringkasan tetap jalan) |
| 107 | Error di script (sintaks, init, `setup()`) |
| 108 | Dihentikan oleh `test.abort()` |

## Tips untuk aplikasi Laravel

- **Login form web:** ambil `_token` dari halaman login lalu kirim bersama form; cookie session tersimpan otomatis per VU dan direset tiap iterasi. Status 419 berarti token CSRF tidak cocok. Lihat `examples/03-form-csrf.js`.
- **API Sanctum/token:** login sekali di `setup()` lalu kirim `Authorization: Bearer ...` (contoh 02), atau login per VU bila ingin menguji endpoint login itu sendiri.
- **Banyak 429:** middleware `throttle` membatasi request dari satu IP. Untuk mengukur kapasitas, longgarkan batasnya di staging.
- **Banyak 5xx:** cek `storage/logs/laravel.log`, log PHP-FPM/Nginx, dan batas koneksi database.
- **Data unik** (registrasi, pesanan): pakai `exec.scenario.iterationInTest` atau `__VU`/`__ITER` untuk email unik.

## Struktur kode

```
main.go             CLI: run, new, demo, version
internal/js         runtime JS per VU (goja), bundel TS/ESM (esbuild), modul k6
internal/engine     opsi & skenario, 6 executor, collector metrik, progress, hasil
internal/metrics    tipe metrik, sink (persentil akurat, memori terbatas), threshold
internal/ui         output terminal: warna, progress, ringkasan
internal/demo       server latihan
```
