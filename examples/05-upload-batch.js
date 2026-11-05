// 05 — Upload file (multipart), request paralel dengan http.batch, dan laporan custom.
// Jalankan: serbu run examples/05-upload-batch.js

import http from 'k6/http';
import { check, sleep } from 'k6';
import { textSummary } from 'https://jslib.k6.io/k6-summary/0.0.2/index.js';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8787';
// open() hanya boleh di init context (di luar function); dibaca sekali per VU.
const gambar = open('./data/contoh.png', 'b');

export const options = {
  vus: 5,
  iterations: 50,
  thresholds: { checks: ['rate==1'] },
};

// 404 di detail produk id 999 memang diharapkan, jadi tidak dihitung sebagai http_req_failed.
const boleh404 = http.expectedStatuses({ min: 200, max: 399 }, 404);

export default function () {
  const up = http.post(`${BASE_URL}/upload`, {
    berkas: http.file(gambar, 'contoh.png', 'image/png'),
    keterangan: 'diunggah oleh serbu',
  });
  check(up, {
    'upload: status 200': (r) => r.status === 200,
    'upload: ukuran sama': (r) => r.json('ukuran') === gambar.byteLength,
  });

  // Tiga request dikirim bersamaan, seperti browser memuat beberapa resource.
  const [daftar, detail, hilang] = http.batch([
    ['GET', `${BASE_URL}/api/produk`],
    ['GET', `${BASE_URL}/api/produk/1`],
    ['GET', `${BASE_URL}/api/produk/999`, null, { responseCallback: boleh404, tags: { name: 'produk-tidak-ada' } }],
  ]);
  check(daftar, { 'batch: daftar 200': (r) => r.status === 200 });
  check(detail, { 'batch: detail 200': (r) => r.status === 200 });
  check(hilang, { 'batch: 404 sesuai harapan': (r) => r.status === 404 });

  sleep(0.5);
}

// Ganti output akhir: tampilkan ringkasan standar + simpan JSON lengkap.
export function handleSummary(data) {
  return {
    stdout: textSummary(data, { indent: '  ', enableColors: data.state.isStdOutTTY }),
    'hasil/ringkasan-upload.json': JSON.stringify(data, null, 2),
  };
}
