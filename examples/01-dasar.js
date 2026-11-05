// 01 — Tes dasar: beban konstan ke halaman daftar & detail produk.
// Jalankan: serbu run examples/01-dasar.js
//           serbu run -u 20 -d 1m -e BASE_URL=https://staging.contoh.com examples/01-dasar.js

import http from 'k6/http';
import { check, sleep } from 'k6';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8787';

export const options = {
  vus: 10,
  duration: '20s',
  thresholds: {
    http_req_failed: ['rate<0.01'],
    http_req_duration: ['p(95)<400'],
    // Threshold khusus untuk request bertag name:detail-produk
    'http_req_duration{name:detail-produk}': ['p(95)<200'],
  },
};

export default function () {
  const daftar = http.get(`${BASE_URL}/api/produk`);
  check(daftar, {
    'daftar: status 200': (r) => r.status === 200,
    'daftar: ada produk': (r) => r.json('total') > 0,
  });

  // URL dinamis (id berbeda-beda) diberi tag name agar dikelompokkan jadi satu di hasil.
  const id = Math.floor(Math.random() * 20) + 1;
  const detail = http.get(`${BASE_URL}/api/produk/${id}`, { tags: { name: 'detail-produk' } });
  check(detail, { 'detail: status 200': (r) => r.status === 200 });

  sleep(1);
}
