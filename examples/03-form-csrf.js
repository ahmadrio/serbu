// 03 — Login lewat form web Laravel: ambil token CSRF, kirim form, ikuti redirect.
// Cookie session tersimpan otomatis per VU (dan direset tiap iterasi).
// Jalankan: serbu run examples/03-form-csrf.js

import http from 'k6/http';
import { check, group, sleep } from 'k6';
// Import jslib k6 ini tetap jalan di serbu (versi bawaan, tanpa unduh).
import { findBetween } from 'https://jslib.k6.io/k6-utils/1.4.0/index.js';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8787';

export const options = {
  vus: 8,
  duration: '20s',
  thresholds: {
    checks: ['rate>0.99'],
    'http_req_duration{group:::login}': ['p(95)<500'],
  },
};

export default function () {
  group('login', () => {
    const page = http.get(`${BASE_URL}/login`);
    const csrf = findBetween(page.body, 'name="_token" value="', '"');
    check(page, { 'halaman login tampil': () => csrf !== null });

    // Objek sebagai body = form-urlencoded, sama seperti form HTML.
    const res = http.post(`${BASE_URL}/login`, {
      _token: csrf,
      email: `user${__VU}@contoh.com`,
      password: 'rahasia',
    });
    check(res, {
      'diarahkan ke dashboard': (r) => r.url.endsWith('/dashboard'),
      'dashboard terbuka': (r) => r.status === 200 && r.body.includes('Selamat datang'),
    });
  });

  sleep(1);
}
