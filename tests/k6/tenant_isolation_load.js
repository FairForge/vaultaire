import http from 'k6/http';
import { check } from 'k6';
import { Rate } from 'k6/metrics';

const errorRate = new Rate('errors');

// Simulate multiple tenants
const tenants = ['tenant-a', 'tenant-b', 'tenant-c', 'tenant-d', 'tenant-e'];

export let options = {
    scenarios: {
        multi_tenant: {
            executor: 'per-vu-iterations',
            vus: 5, // One VU per tenant
            iterations: 100,
            maxDuration: '5m',
        },
    },
    thresholds: {
        http_req_duration: ['p(95)<500'],
        errors: ['rate<0.01'],
    },
};

export default function() {
    const tenantId = tenants[__VU - 1]; // Each VU gets a different tenant
    const bucket = `${tenantId}-bucket`;
    const data = `Data for ${tenantId}: ${'x'.repeat(10000)}`;

    // Each tenant writes to their own namespace
    for (let i = 0; i < 10; i++) {
        const key = `file-${__ITER}-${i}.txt`;

        // Write with tenant header
        const putRes = http.put(
            `http://localhost:8000/${bucket}/${key}`,
            data,
            {
                headers: {
                    'X-Tenant-ID': tenantId,
                    'Content-Type': 'text/plain'
                },
                tags: { tenant: tenantId, operation: 'PUT' }
            }
        );

        check(putRes, {
            [`${tenantId} PUT success`]: (r) => r.status === 200,
        });
        errorRate.add(putRes.status !== 200);

        // Read back to verify isolation
        const getRes = http.get(
            `http://localhost:8000/${bucket}/${key}`,
            {
                headers: { 'X-Tenant-ID': tenantId },
                tags: { tenant: tenantId, operation: 'GET' }
            }
        );

        check(getRes, {
            [`${tenantId} GET success`]: (r) => r.status === 200,
            [`${tenantId} data isolated`]: (r) => r.body.includes(`Data for ${tenantId}`),
        });
        errorRate.add(getRes.status !== 200);
    }
}
