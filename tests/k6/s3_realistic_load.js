import http from 'k6/http';
import { check } from 'k6';
import { SharedArray } from 'k6/data';
import { Rate } from 'k6/metrics';

const errorRate = new Rate('errors');

// Simulate different file sizes
const fileSizes = new SharedArray('sizes', function() {
    return [
        1024,        // 1KB - 60% of requests
        10240,       // 10KB - 30% of requests
        102400,      // 100KB - 9% of requests
        1048576,     // 1MB - 1% of requests
    ];
});

const sizeWeights = [60, 30, 9, 1];

export let options = {
    scenarios: {
        normal_load: {
            executor: 'ramping-arrival-rate',
            startRate: 10,
            timeUnit: '1s',
            preAllocatedVUs: 50,
            maxVUs: 100,
            stages: [
                { duration: '2m', target: 10 },  // Warm up
                { duration: '5m', target: 50 },  // Ramp to 50 req/s
                { duration: '10m', target: 50 }, // Stay at 50 req/s
                { duration: '2m', target: 0 },   // Ramp down
            ],
        },
    },
    thresholds: {
        http_req_duration: ['p(95)<1000', 'p(99)<2000'],
        errors: ['rate<0.01'],
    },
};

function getWeightedSize() {
    const rand = Math.random() * 100;
    let cumulative = 0;
    for (let i = 0; i < sizeWeights.length; i++) {
        cumulative += sizeWeights[i];
        if (rand < cumulative) {
            return fileSizes[i];
        }
    }
    return fileSizes[0];
}

export default function() {
    const size = getWeightedSize();
    const data = 'x'.repeat(size);
    const bucket = 'k6-load-test';
    const key = `file-${Date.now()}-${Math.random()}.dat`;

    // 70% reads, 25% writes, 5% lists
    const operation = Math.random();

    if (operation < 0.25) {
        // Write operation
        const res = http.put(
            `http://localhost:8000/${bucket}/${key}`,
            data,
            { headers: { 'Content-Type': 'application/octet-stream' } }
        );
        check(res, { 'PUT success': (r) => r.status === 200 });
        errorRate.add(res.status !== 200);

    } else if (operation < 0.95) {
        // Read operation (need existing keys - simplified for demo)
        const res = http.get(`http://localhost:8000/${bucket}/sample.dat`);
        check(res, { 'GET success': (r) => r.status === 200 });
        errorRate.add(res.status !== 200);

    } else {
        // List operation
        const res = http.get(`http://localhost:8000/${bucket}/`);
        check(res, { 'LIST success': (r) => r.status === 200 });
        errorRate.add(res.status !== 200);
    }
}
