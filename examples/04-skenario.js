// 04 — Beberapa skenario sekaligus: pengunjung dengan laju tetap + pembeli yang naik-turun.
// Arrival-rate menjaga laju iterasi per detik walau server melambat (model "open"),
// cocok untuk menjawab: "kuat tidak kalau ada 20 pengunjung per detik?"
// Jalankan: serbu run examples/04-skenario.js

import http from 'k6/http';
import { check, sleep } from 'k6';
import exec from 'k6/execution';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8787';

export const options = {
  scenarios: {
    pengunjung: {
      executor: 'constant-arrival-rate',
      rate: 20, // 20 iterasi per detik
      timeUnit: '1s',
      duration: '30s',
      preAllocatedVUs: 10,
      maxVUs: 40,
      exec: 'lihatKatalog',
    },
    pembeli: {
      executor: 'ramping-vus',
      startTime: '5s',
      startVUs: 0,
      stages: [
        { duration: '10s', target: 5 },
        { duration: '10s', target: 5 },
        { duration: '5s', target: 0 },
      ],
      exec: 'beli',
      tags: { peran: 'pembeli' },
    },
  },
  thresholds: {
    // Hentikan tes lebih awal bila error > 5% (dievaluasi setelah 10 detik).
    http_req_failed: [{ threshold: 'rate<0.05', abortOnFail: true, delayAbortEval: '10s' }],
    'http_req_duration{scenario:pengunjung}': ['p(95)<300'],
    'http_req_duration{peran:pembeli}': ['p(95)<500'],
  },
};

export function lihatKatalog() {
  const res = http.get(`${BASE_URL}/api/produk?q=a`);
  check(res, { 'katalog: status 200': (r) => r.status === 200 });
}

export function beli() {
  // Email unik per iterasi di seluruh tes, berguna untuk endpoint registrasi.
  const email = `pembeli-${exec.vu.idInTest}-${exec.scenario.iterationInTest}@contoh.com`;
  const login = http.post(`${BASE_URL}/api/login`, JSON.stringify({ email, password: 'rahasia' }), {
    headers: { 'Content-Type': 'application/json' },
  });
  const token = login.json('token');
  const res = http.post(`${BASE_URL}/api/pesanan`, JSON.stringify({ produk_id: 3, jumlah: 1 }), {
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` },
  });
  check(res, { 'beli: status 201': (r) => r.status === 201 });
  sleep(2);
}
