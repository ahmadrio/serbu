// 02 — API dengan login token (mirip Laravel Sanctum), ditulis dalam TypeScript.
// setup() login sekali, token dibagikan ke semua VU lewat parameter `data`.
// Jalankan: serbu run examples/02-api-login.ts
//           serbu run -e BASE_URL=https://api.staging.contoh.com -e EMAIL=uji@contoh.com -e PASSWORD=... examples/02-api-login.ts

import http from 'k6/http';
import { check, group, sleep } from 'k6';
import { Counter, Trend } from 'k6/metrics';
import { BASE_URL, jsonHeaders } from './lib/api.ts';

const waktuPesan = new Trend('waktu_buat_pesanan', true);
const pesananDibuat = new Counter('pesanan_dibuat');

export const options = {
  stages: [
    { duration: '10s', target: 15 },
    { duration: '20s', target: 15 },
    { duration: '5s', target: 0 },
  ],
  thresholds: {
    http_req_failed: ['rate<0.01'],
    checks: ['rate>0.99'],
    'http_req_duration{name:buat-pesanan}': ['p(95)<300'],
    waktu_buat_pesanan: ['avg<200'],
    pesanan_dibuat: ['count>50'],
  },
};

interface SetupData {
  token: string;
}

export function setup(): SetupData {
  const res = http.post(
    `${BASE_URL}/api/login`,
    JSON.stringify({ email: __ENV.EMAIL || 'uji@contoh.com', password: __ENV.PASSWORD || 'rahasia' }),
    { headers: jsonHeaders() },
  );
  if (res.status !== 200) {
    throw new Error(`Login gagal (status ${res.status}): ${res.body}`);
  }
  return { token: res.json('token') as string };
}

export default function (data: SetupData): void {
  group('lihat profil', () => {
    const res = http.get(`${BASE_URL}/api/profil`, { headers: jsonHeaders(data.token) });
    check(res, { 'profil: status 200': (r) => r.status === 200 });
  });

  group('buat pesanan', () => {
    const body = JSON.stringify({ produk_id: Math.floor(Math.random() * 20) + 1, jumlah: 2 });
    const res = http.post(`${BASE_URL}/api/pesanan`, body, {
      headers: jsonHeaders(data.token),
      tags: { name: 'buat-pesanan' },
    });
    const ok = check(res, {
      'pesanan: status 201': (r) => r.status === 201,
      'pesanan: ada id': (r) => typeof r.json('data.id') === 'number',
    });
    waktuPesan.add(res.timings.duration);
    if (ok) pesananDibuat.add(1);
  });

  sleep(1);
}

export function teardown(data: SetupData): void {
  console.log(`tes selesai, token yang dipakai: ${data.token.slice(0, 8)}…`);
}
