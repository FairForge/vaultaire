// The mirror-settle lab's backends (RP78-2, Worker 0.6.0): one Node server,
// three sinks and the settle recorder.
//   PUT /primary/<k>   reads at full speed, ETag = MD5
//   PUT /slow/<k>      reads at ~2 MB/s, ETag = MD5
//   PUT /stall/<k>     reads every byte, answers after 40 s
// Every PUT without a Content-Length is 411 (iDrive's answer to 0.5.1).
//   POST /api/v1/edge/mirror   records the settle (the control plane's route)
//   GET  /log                  what each sink saw, as JSON
import { createServer } from "node:http";
import { createHash } from "node:crypto";

const log = [];
const port = Number(process.env.SINK_PORT || 8788);
createServer((req, res) => {
  const t0 = Date.now();
  if (req.method === "GET" && req.url === "/log") {
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify(log));
    return;
  }
  if (req.method === "POST" && req.url === "/api/v1/edge/mirror") {
    let b = "";
    req.on("data", (d) => { b += d; });
    req.on("end", () => {
      log.push({ sink: "settle", at: Date.now(), body: JSON.parse(b || "{}") });
      res.writeHead(200, { "content-type": "application/json" });
      res.end('{"state":"ready"}');
    });
    return;
  }
  const sink = req.url.split("/")[1];
  const cl = req.headers["content-length"];
  if (cl === undefined) {
    log.push({ sink, at: Date.now(), status: 411, chunked: req.headers["transfer-encoding"] || "" });
    res.writeHead(411);
    res.end();
    req.resume();
    return;
  }
  const md5 = createHash("md5");
  let n = 0;
  req.on("data", (d) => {
    md5.update(d);
    n += d.length;
    if (sink === "slow") {
      req.pause();
      setTimeout(() => req.resume(), Math.ceil((d.length / (2 << 20)) * 1000));
    }
  });
  req.on("end", () => {
    const etag = `"${md5.digest("hex")}"`;
    const answer = () => {
      log.push({ sink, at: Date.now(), status: 200, bytes: n, content_length: Number(cl), ms: Date.now() - t0, etag });
      if (!res.writableEnded && !res.destroyed) {
        res.writeHead(200, { etag });
        res.end();
      }
    };
    if (sink === "stall") setTimeout(answer, 40_000);
    else answer();
  });
  req.on("error", () => log.push({ sink, at: Date.now(), status: 0, bytes: n, aborted: true }));
  req.on("aborted", () => log.push({ sink, at: Date.now(), status: 0, bytes: n, aborted: true }));
}).listen(port, "127.0.0.1", () => console.log(`sink on ${port}`));
