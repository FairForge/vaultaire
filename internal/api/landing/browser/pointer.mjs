// Real-input checks for the landing page's drag: synthetic events dispatched
// at an element (harness.html) cannot see a lost pointer capture, which is
// exactly how pieces got stuck "in hand" after a click. This drives Chrome
// through the DevTools protocol with Input.dispatchMouseEvent, so the browser
// decides where every move and release lands. Prints PASS/FAIL lines; exit 1
// on any FAIL. Usage: node pointer.mjs <closed.html> <chrome binary>
import { spawn } from 'node:child_process';

const [file, chrome] = process.argv.slice(2);
const port = 9222 + (process.pid % 500);
const proc = spawn(chrome, ['--headless=new', '--disable-gpu', '--no-sandbox', '--allow-file-access-from-files',
    `--remote-debugging-port=${port}`, '--window-size=1280,1600', 'about:blank'], { stdio: 'ignore' });
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const out = [];
const ok = (cond, name, detail) => out.push((cond ? 'PASS ' : 'FAIL ') + name + (detail !== undefined ? ': ' + detail : ''));

try {
    let targets;
    for (let i = 0; i < 80 && !targets; i++) {
        try { targets = await (await fetch(`http://127.0.0.1:${port}/json`)).json(); } catch { await sleep(250); }
    }
    const page = targets.find((t) => t.type === 'page');
    const ws = new WebSocket(page.webSocketDebuggerUrl);
    await new Promise((r) => { ws.onopen = r; });
    let id = 0; const pending = new Map();
    ws.onmessage = (m) => { const d = JSON.parse(m.data); if (d.id && pending.has(d.id)) { pending.get(d.id)(d); pending.delete(d.id); } };
    const send = (method, params = {}) => new Promise((r) => { const i = ++id; pending.set(i, r); ws.send(JSON.stringify({ id: i, method, params })); });
    const js = async (expr) => (await send('Runtime.evaluate', { expression: expr, returnByValue: true })).result.result.value;
    const mouse = (type, x, y, extra = {}) => send('Input.dispatchMouseEvent', { type, x, y, button: 'left', ...extra });
    const press = (x, y) => mouse('mousePressed', x, y, { clickCount: 1 });
    const release = (x, y) => mouse('mouseReleased', x, y, { clickCount: 1 });
    const dragTo = (x, y) => mouse('mouseMoved', x, y, { button: 'left', buttons: 1 });
    const hover = (x, y) => mouse('mouseMoved', x, y, { button: 'none' });

    await send('Page.enable');
    await send('Page.navigate', { url: 'file://' + file });
    await sleep(2500);
    await js(`document.head.appendChild(Object.assign(document.createElement('style'), { textContent: '*{transition:none!important;animation:none!important}' }));
        try { localStorage.clear(); } catch (e) {}
        document.querySelector('svg.scene[data-play] .piece[transform="translate(66 13)"]').id = 'probe'; 'ok'`);
    const probe = () => js(`(() => { const g = document.getElementById('probe'), b = g.getBoundingClientRect();
        return { t: g.getAttribute('transform'), lift: g.classList.contains('lift'), x: b.left + b.width / 2, y: b.top + b.height / 2 }; })()`);

    // ---- hero: a click leaves the piece where it is and lets go of it
    let p = await probe();
    await hover(p.x, p.y); await press(p.x, p.y); await release(p.x, p.y); await sleep(80);
    let q = await probe();
    ok(q.t === p.t && !q.lift, 'click leaves the piece in place and lets go', q.t + ' lift=' + q.lift);
    await hover(q.x + 80, q.y + 30); await sleep(80);
    let r = await probe();
    ok(r.t === q.t && !r.lift, 'hovering after a click does not move it', r.t);

    // ---- hero: a drag moves it, the drop lets go, hovering afterwards is inert
    await hover(r.x, r.y); await press(r.x, r.y); await dragTo(r.x - 40, r.y - 20); await dragTo(r.x - 70, r.y - 40); await release(r.x - 70, r.y - 40); await sleep(80);
    let d = await probe();
    ok(d.t !== r.t && !d.lift, 'drag moves the piece and the drop lets go', d.t + ' lift=' + d.lift);
    await hover(d.x + 60, d.y + 10); await sleep(80);
    let h = await probe();
    ok(h.t === d.t && !h.lift, 'hovering after a drag does not move it', h.t);

    // ---- builder room: the same with a storage piece, read off the receipt
    // the page scrolls smoothly: scroll instantly, then read the rect
    await js(`document.documentElement.style.scrollBehavior = 'auto'; document.querySelector('#room-items .piece[aria-label^="Storage box: photos"]').scrollIntoView({ block: 'center', behavior: 'instant' }); 'ok'`);
    await sleep(300);
    const room = await js(`(() => { const g = document.querySelector('#room-items .piece[aria-label^="Storage box: photos"]'), b = g.getBoundingClientRect();
        return { x: b.left + b.width / 2, y: b.top + b.height / 2, total: document.getElementById('r-total').textContent, u: document.getElementById('room').getBoundingClientRect().height / 115, scrollY: window.scrollY }; })()`);
    ok(room.scrollY > 0 && room.y > 0 && room.y < 1600, 'room scrolled into view', room.scrollY + ' y=' + Math.round(room.y));
    await hover(room.x, room.y); await press(room.x, room.y); await dragTo(room.x, room.y - 30 * room.u); await dragTo(room.x, room.y - 60 * room.u); await release(room.x, room.y - 60 * room.u); await sleep(120);
    const after = await js(`(() => ({ total: document.getElementById('r-total').textContent, lifted: document.querySelectorAll('#room-items .piece.lift').length }))()`);
    ok(after.total !== room.total && after.lifted === 0, 'room: a real drag to the attic re-prices and lets go', room.total + ' -> ' + after.total + ' lifted=' + after.lifted);
} catch (x) {
    out.push('FAIL exception: ' + (x.stack || x));
} finally {
    proc.kill();
}
console.log(out.join('\n'));
process.exit(out.some((l) => l.startsWith('FAIL')) ? 1 : 0);
