//! Edge erasure decode for the Worker bench: plain C ABI, no bindgen.
//! Memory protocol: JS calls alloc(n) for a buffer, writes shards back to back
//! (n slots × shard_len, missing slots left as-is), passes a present bitmask,
//! and gets the first `size` bytes of the payload written into `out`.
use reed_solomon_erasure::galois_8::ReedSolomon;
use raptorq::{Decoder, EncodingPacket, ObjectTransmissionInformation};
use std::alloc::{alloc, dealloc, Layout};

#[no_mangle]
pub extern "C" fn ec_alloc(n: usize) -> *mut u8 {
    unsafe { alloc(Layout::from_size_align(n.max(1), 8).unwrap()) }
}

#[no_mangle]
pub extern "C" fn ec_free(p: *mut u8, n: usize) {
    unsafe { dealloc(p, Layout::from_size_align(n.max(1), 8).unwrap()) }
}

/// Reed-Solomon: k data + m parity shards of shard_len bytes in `shards`
/// (slot i at i*shard_len), `present` bit i set when slot i was fetched.
/// Writes `size` bytes to `out`. Returns 0 on success.
#[no_mangle]
pub extern "C" fn rs_decode(k: usize, m: usize, shard_len: usize, shards: *mut u8, present: u32, out: *mut u8, size: usize) -> i32 {
    let n = k + m;
    let rs = match ReedSolomon::new(k, m) { Ok(r) => r, Err(_) => return 1 };
    let all = unsafe { std::slice::from_raw_parts_mut(shards, n * shard_len) };
    let mut opts: Vec<Option<Vec<u8>>> = Vec::with_capacity(n);
    for i in 0..n {
        if present & (1 << i) != 0 { opts.push(Some(all[i * shard_len..(i + 1) * shard_len].to_vec())); } else { opts.push(None); }
    }
    if rs.reconstruct_data(&mut opts).is_err() { return 2; }
    let o = unsafe { std::slice::from_raw_parts_mut(out, size) };
    let mut off = 0;
    for i in 0..k {
        let s = opts[i].as_ref().unwrap();
        let take = (size - off).min(shard_len);
        o[off..off + take].copy_from_slice(&s[..take]);
        off += take;
        if off >= size { break; }
    }
    0
}

/// RaptorQ (RFC 6330), symbols dealt round-robin to n shards: symbol ESI e sits
/// at position e / n of shard e % n (the layout erasure-bench writes). `symbol`
/// is T; `size` the transfer length. Decodes from whichever shards are present.
#[no_mangle]
pub extern "C" fn rq_decode(k: usize, m: usize, shard_len: usize, symbol: usize, shards: *mut u8, present: u32, out: *mut u8, size: usize) -> i32 {
    let n = k + m;
    let per_shard = shard_len / symbol;
    let oti = ObjectTransmissionInformation::with_defaults(size as u64, symbol as u16);
    let mut dec = Decoder::new(oti);
    let all = unsafe { std::slice::from_raw_parts(shards, n * shard_len) };
    let mut result: Option<Vec<u8>> = None;
    'outer: for pos in 0..per_shard {
        for sh in 0..n {
            if present & (1 << sh) == 0 { continue; }
            let esi = (pos * n + sh) as u32;
            let off = sh * shard_len + pos * symbol;
            let pkt = EncodingPacket::new(raptorq::PayloadId::new(0, esi), all[off..off + symbol].to_vec());
            if let Some(data) = dec.decode(pkt) { result = Some(data); break 'outer; }
        }
    }
    match result {
        Some(data) if data.len() >= size => {
            let o = unsafe { std::slice::from_raw_parts_mut(out, size) };
            o.copy_from_slice(&data[..size]);
            0
        }
        _ => 3,
    }
}

/// Reed-Solomon reconstruct IN PLACE: missing data slots (bit clear in
/// `present`, index < k) are rebuilt into their own slot of `shards`, so the
/// payload is then the contiguous bytes of slots 0..k (klauspost's contiguous
/// Split layout) and can be streamed straight out of this memory. Returns 0.
#[no_mangle]
pub extern "C" fn rs_reconstruct(k: usize, m: usize, shard_len: usize, shards: *mut u8, present: u32) -> i32 {
    let n = k + m;
    let rs = match ReedSolomon::new(k, m) { Ok(r) => r, Err(_) => return 1 };
    let all = unsafe { std::slice::from_raw_parts_mut(shards, n * shard_len) };
    let mut opts: Vec<Option<Vec<u8>>> = Vec::with_capacity(n);
    for i in 0..n {
        if present & (1 << i) != 0 { opts.push(Some(all[i * shard_len..(i + 1) * shard_len].to_vec())); } else { opts.push(None); }
    }
    if rs.reconstruct_data(&mut opts).is_err() { return 2; }
    for i in 0..k {
        if present & (1 << i) == 0 {
            all[i * shard_len..(i + 1) * shard_len].copy_from_slice(opts[i].as_ref().unwrap());
        }
    }
    0
}
