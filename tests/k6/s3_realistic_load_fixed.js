import http from 'k6/http';
import { check } from 'k6';
import { SharedArray } from 'k6/data';
import { Rate } from 'k6/metrics';

const errorRate = new Rate('errors');

// Store created keys for reading
const createdKeys = [];

const fileSizes = new SharedArray('sizes', function() {
    return [1024, 10240, 102400, 1048576];
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
                { duration: '1m', target: 10 },  // Warm up
                { duration: '3m', target: 30 },  // Ramp to 30 req/s
                { duration: '5m', target: 30 },  // Stay at 30 req/s
                { duration: '1m', target: 0 },   // Ramp down
            ],
        },
    },
    thresholds: {
        http_req_duration: ['p(95)<1000', 'p(99)<2000'],
        errors: ['rate<0.05'],
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
    const bucket = 'k6-load-test';
    const operation = Math.random();

    if (operation < 0.25 || createdKeys.length === 0) {
        // Write operation (or forced write if no keys exist yet)
        const size = getWeightedSize();
        const data = 'x'.repeat(size);
        const key = `file-${Date.now()}-${Math.random()}.dat`;

        const res = http.put(
            `http://localhost:8000/${bucket}/${key}`,
            data,
            { headers: { 'Content-Type': 'application/octet-stream' } }
        );

        if (check(res, { 'PUT success': (r) => r.status === 200 })) {
            // Only add to read list if write succeeded
            if (createdKeys.length < 1000) { // Limit memory usage
                createdKeys.push(key);
            }
        }
        errorRate.add(res.status !== 200);

    } else if (operation < 0.95 && createdKeys.length > 0) {
        // Read operation - use a random created key
        const key = createdKeys[Math.floor(Math.random() * createdKeys.length)];
        const res = http.get(`http://localhost:8000/${bucket}/${key}`);
        check(res, { 'GET success': (r) => r.status === 200 });
        errorRate.add(res.status !== 200);

    } else {
        // List operation
        const res = http.get(`http://localhost:8000/${bucket}/`);
        check(res, { 'LIST success': (r) => r.status === 200 });
        errorRate.add(res.status !== 200);
    }
}
