import http from 'k6/http';
import { check } from 'k6';
import { Trend } from 'k6/metrics';

// Custom metrics for resource monitoring
const memoryUsage = new Trend('memory_mb');
const cpuUsage = new Trend('cpu_percent');
const goroutines = new Trend('goroutines');

export let options = {
    scenarios: {
        load_with_monitoring: {
            executor: 'constant-arrival-rate',
            rate: 20,
            timeUnit: '1s',
            duration: '3m',
            preAllocatedVUs: 10,
        },
        monitor_resources: {
            executor: 'constant-arrival-rate',
            rate: 1,
            timeUnit: '1s',
            duration: '3m',
            preAllocatedVUs: 1,
            exec: 'monitorResources',
        },
    },
};

export default function() {
    // Normal load operations
    const res = http.put(
        `http://localhost:8000/test-bucket/file-${__ITER}.txt`,
        'x'.repeat(10000)
    );
    check(res, { 'PUT success': (r) => r.status === 200 });
}

export function monitorResources() {
    // Poll the metrics endpoint
    const res = http.get('http://localhost:8000/metrics');

    if (res.status === 200) {
        // Parse Prometheus-style metrics
        const lines = res.body.split('\n');
        lines.forEach(line => {
            if (line.includes('process_resident_memory_bytes')) {
                const value = parseFloat(line.split(' ')[1]);
                memoryUsage.add(value / 1024 / 1024); // Convert to MB
            }
            // Add more metrics as needed
        });
    }

    // Also check runtime stats endpoint if available
    const statsRes = http.get('http://localhost:8000/debug/vars');
    if (statsRes.status === 200) {
        const stats = JSON.parse(statsRes.body);
        if (stats.memstats) {
            memoryUsage.add(stats.memstats.Alloc / 1024 / 1024);
        }
    }
}
