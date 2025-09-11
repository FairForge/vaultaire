import http from 'k6/http';
import { check, sleep } from 'k6';
import { Rate } from 'k6/metrics';

const errorRate = new Rate('errors');

export let options = {
    stages: [
        { duration: '30s', target: 5 },   // Ramp up to 5 users
        { duration: '1m', target: 5 },    // Stay at 5 users
        { duration: '30s', target: 0 },   // Ramp down
    ],
    thresholds: {
        http_req_duration: ['p(95)<500'], // 95% of requests under 500ms
        errors: ['rate<0.1'],              // Error rate under 10%
    },
};

export default function() {
    const bucket = 'k6-test';
    const key = `file-${__VU}-${__ITER}.txt`;
    const data = 'x'.repeat(10000); // 10KB

    // PUT operation
    const putRes = http.put(
        `http://localhost:8000/${bucket}/${key}`,
        data,
        {
            headers: { 'Content-Type': 'text/plain' },
            tags: { operation: 'PUT' }
        }
    );

    check(putRes, {
        'PUT status is 200': (r) => r.status === 200,
    });
    errorRate.add(putRes.status !== 200);

    sleep(0.1);

    // GET operation
    const getRes = http.get(
        `http://localhost:8000/${bucket}/${key}`,
        { tags: { operation: 'GET' } }
    );

    check(getRes, {
        'GET status is 200': (r) => r.status === 200,
        'GET returns correct data': (r) => r.body === data,
    });
    errorRate.add(getRes.status !== 200);

    sleep(0.1);
}
