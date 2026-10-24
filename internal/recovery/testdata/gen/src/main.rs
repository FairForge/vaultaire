//! Golden vectors for internal/recovery (K5): every derivation, proof, share,
//! Noise message and wire encoding the Go implementation must reproduce
//! byte for byte. Everything random is drawn from a recorded entropy tape
//! (`entropy` in each vector), so the Go side consumes the same bytes in the
//! same order. The juicebox crates are the MIT SDK at the pinned commit; the
//! few SDK functions that are `pub(crate)` (PIN hashing, the user-secret key
//! and cipher, the unlock key split) are re-stated here line for line from
//! rust/sdk/src/{pin.rs,types.rs} and checked against the SDK's own test
//! constants before anything is written.

use std::collections::VecDeque;
use std::time::Duration;

use argon2::{Algorithm, Argon2, Params, ParamsBuilder, Version};
use blake2::Blake2sMac256;
use chacha20poly1305::aead::Aead;
use chacha20poly1305::ChaCha20Poly1305;
use curve25519_dalek::{RistrettoPoint, Scalar};
use digest::{Digest, KeyInit, Mac};
use rand_core::{CryptoRng, RngCore};
use serde_json::{json, Value};
use sha2::Sha512;
use x25519_dalek as x25519;

use juicebox_marshalling::{self as marshalling, to_be4};
use juicebox_noise::{client as nclient, server as nserver};
use juicebox_oprf as oprf;
use juicebox_realm_api::requests::{
    ClientRequest, ClientRequestKind, ClientResponse, DeleteResponse, NoiseRequest,
    NoiseResponse, PaddedSecretsResponse, Recover1Response, Recover2Request, Recover2Response,
    Recover3Request, Recover3Response, Register1Response, Register2Request, Register2Response,
    SecretsRequest, SecretsResponse,
};
use juicebox_realm_api::signing::{sign_public_key, OprfSigningKey};
use juicebox_realm_api::types::{
    AuthToken, EncryptedUserSecret, EncryptedUserSecretCommitment, Policy, RealmId,
    RegistrationVersion, SessionId, UnlockKey, UnlockKeyCommitment, UnlockKeyTag,
    UserSecretEncryptionKeyScalarShare,
};
use juicebox_secret_sharing::{create_shares, recover_secret, Index, Share};

/// An entropy tape: every byte a constructor draws is taken from the front.
struct Tape {
    bytes: VecDeque<u8>,
}

impl Tape {
    /// A deterministic tape: SHA-512(label ‖ be4(counter)) blocks.
    fn new(label: &str, len: usize) -> (Self, String) {
        let mut out = Vec::with_capacity(len + 64);
        let mut counter = 0u32;
        while out.len() < len {
            out.extend_from_slice(
                &Sha512::new()
                    .chain_update(label.as_bytes())
                    .chain_update(counter.to_be_bytes())
                    .finalize(),
            );
            counter += 1;
        }
        out.truncate(len);
        let hexed = hex::encode(&out);
        (
            Tape {
                bytes: out.into_iter().collect(),
            },
            hexed,
        )
    }

    fn assert_empty(&self, what: &str) {
        assert!(self.bytes.is_empty(), "{what}: {} entropy bytes unused", self.bytes.len());
    }
}

impl RngCore for Tape {
    fn next_u32(&mut self) -> u32 {
        rand_core::impls::next_u32_via_fill(self)
    }
    fn next_u64(&mut self) -> u64 {
        rand_core::impls::next_u64_via_fill(self)
    }
    fn fill_bytes(&mut self, dest: &mut [u8]) {
        assert!(self.bytes.len() >= dest.len(), "entropy tape too short");
        for b in dest.iter_mut() {
            *b = self.bytes.pop_front().unwrap();
        }
    }
    fn try_fill_bytes(&mut self, dest: &mut [u8]) -> Result<(), rand_core::Error> {
        self.fill_bytes(dest);
        Ok(())
    }
}

impl CryptoRng for Tape {}

fn cbor<T: serde::Serialize>(v: &T) -> String {
    hex::encode(marshalling::to_vec(v).expect("cbor"))
}

/// The byte string inside a 32-byte CBOR bytes item (0x58 0x20 ‖ 32 bytes).
fn cbor_bytes32<T: serde::Serialize>(v: &T) -> String {
    let b = marshalling::to_vec(v).unwrap();
    assert_eq!(&b[..2], &[0x58, 0x20]);
    hex::encode(&b[2..])
}

fn h(b: &[u8]) -> String {
    hex::encode(b)
}

// ---- re-stated from rust/sdk/src/pin.rs (pub(crate) there) ----
fn pin_hash(mode: &str, pin: &[u8], version: &[u8; 16], info: &[u8]) -> ([u8; 32], [u8; 32]) {
    let params = match mode {
        "Standard2019" => ParamsBuilder::new()
            .m_cost(1024 * 16)
            .t_cost(32)
            .p_cost(1)
            .build()
            .unwrap(),
        "FastInsecure" => ParamsBuilder::new()
            .m_cost(Params::MIN_M_COST)
            .t_cost(Params::MIN_T_COST)
            .p_cost(Params::MIN_P_COST)
            .build()
            .unwrap(),
        _ => unreachable!(),
    };
    let mut out = [0u8; 64];
    Argon2::new(Algorithm::Argon2id, Version::V0x13, params)
        .hash_password_into(
            pin,
            [
                &to_be4(version.len()),
                version.as_slice(),
                &to_be4(info.len()),
                info,
            ]
            .concat()
            .as_slice(),
            &mut out,
        )
        .unwrap();
    (out[..32].try_into().unwrap(), out[32..].try_into().unwrap())
}

// ---- re-stated from rust/sdk/src/types.rs (pub(crate) there) ----
fn encryption_key(seed: &[u8; 32], scalar: &Scalar) -> [u8; 32] {
    let label = b"User Secret Encryption Key";
    <Blake2sMac256 as Mac>::new(seed.into())
        .chain_update(to_be4(label.len()))
        .chain_update(label)
        .chain_update(to_be4(scalar.as_bytes().len()))
        .chain_update(scalar.as_bytes())
        .finalize()
        .into_bytes()
        .into()
}

fn encrypt_secret(key: &[u8; 32], secret: &[u8]) -> [u8; 145] {
    assert!(secret.len() <= 128);
    let mut padded = secret.to_vec();
    padded.insert(0, secret.len() as u8);
    padded.resize(129, 0);
    let ct = ChaCha20Poly1305::new(key.into())
        .encrypt(&[0u8; 12].into(), padded.as_slice())
        .unwrap();
    ct.try_into().unwrap()
}

fn unlock_key_and_commitment(output: &oprf::Output) -> ([u8; 32], [u8; 32]) {
    let d: [u8; 64] = Sha512::digest(output.expose_secret()).into();
    (d[32..].try_into().unwrap(), d[..32].try_into().unwrap())
}

fn main() {
    // The SDK's own constants (rust/sdk/src/pin.rs test_pin_hashing,
    // rust/sdk/src/types.rs test_secret_encryption) — the re-statements above
    // must reproduce them, or nothing is written.
    let (ak, seed) = pin_hash("Standard2019", b"1234", &[5u8; 16], b"artemis");
    assert_eq!(
        ak,
        [
            41, 53, 218, 132, 201, 116, 35, 179, 127, 52, 87, 35, 27, 135, 124, 230, 172, 32,
            147, 80, 29, 114, 85, 219, 238, 29, 235, 9, 165, 216, 130, 27
        ]
    );
    assert_eq!(
        seed,
        [
            135, 200, 201, 181, 211, 234, 159, 234, 131, 182, 172, 106, 100, 226, 91, 151, 196,
            114, 44, 164, 228, 11, 234, 37, 35, 239, 234, 38, 33, 37, 226, 42
        ]
    );
    let sdk_ct = encrypt_secret(&[8u8; 32], b"artemis");
    assert_eq!(&sdk_ct[..4], &[1, 134, 178, 251]);
    assert_eq!(&sdk_ct[141..], &[109, 107, 35, 7]);

    let mut root = serde_json::Map::new();
    root.insert(
        "source".into(),
        json!({
            "juicebox_sdk_commit": "2e0f544e3fd0bd0997a034f73c8efb6fade3690c",
            "generator": "internal/recovery/testdata/gen (cargo run --release)",
            "entropy_tape": "SHA-512(label || be32(counter)) blocks, consumed front to back"
        }),
    );

    // ---- OPRF (2HashDH) + DLEQ ----
    let mut oprf_vectors = vec![];
    for i in 0..8u32 {
        let label = format!("oprf-{i}");
        let (mut tape, entropy) = Tape::new(&label, 64 * 3);
        let input: Vec<u8> = (0..(i * 7) as u8).collect();
        let sk = oprf::PrivateKey::random(&mut tape);
        let pk = sk.to_public_key();
        let (bf, bi) = oprf::start(&input, &mut tape);
        let (bo, proof) = oprf::blind_verifiable_evaluate(&sk, &pk, &bi, &mut tape);
        tape.assert_empty(&label);
        assert!(oprf::verify_proof(&bi, &bo, &pk, &proof).is_ok());
        let out = oprf::finalize(&input, &bf, &bo);
        assert_eq!(out, oprf::unoblivious_evaluate(&sk, &input));
        oprf_vectors.push(json!({
            "name": label,
            "entropy": entropy,
            "input": h(&input),
            "private_key": h(sk.expose_secret().as_bytes()),
            "public_key": h(pk.as_bytes()),
            "blinded_input": cbor_bytes32(&bi),
            "blinded_output": cbor_bytes32(&bo),
            "proof_cbor": cbor(&proof),
            "output": h(out.expose_secret()),
        }));
        drop(bf);
    }
    root.insert("oprf".into(), Value::Array(oprf_vectors));

    // ---- Shamir over scalars and over points (Lagrange at 0) ----
    let mut sss = vec![];
    for (threshold, count) in [(1u32, 1u32), (2, 2), (2, 3), (3, 5)] {
        let label = format!("sss-scalar-{threshold}-of-{count}");
        let (mut tape, entropy) = Tape::new(&label, 64 * threshold as usize);
        let secret = Scalar::random(&mut tape);
        let shares: Vec<Share<Scalar>> =
            create_shares(&secret, threshold, count, &mut tape).collect();
        tape.assert_empty(&label);
        assert_eq!(recover_secret(&shares[..threshold as usize]).unwrap(), secret);
        sss.push(json!({
            "name": label, "kind": "scalar", "threshold": threshold, "count": count,
            "entropy": entropy,
            "secret": h(secret.as_bytes()),
            "shares": shares.iter().map(|s| json!({"index": s.index.0, "secret": h(s.secret.as_bytes())})).collect::<Vec<_>>(),
        }));

        let label = format!("sss-point-{threshold}-of-{count}");
        let (mut tape, entropy) = Tape::new(&label, 64 * threshold as usize);
        let secret = RistrettoPoint::random(&mut tape);
        let shares: Vec<Share<RistrettoPoint>> =
            create_shares(&secret, threshold, count, &mut tape).collect();
        tape.assert_empty(&label);
        // any `threshold` of them, from the back, recover it too
        let tail: Vec<_> = shares[(count - threshold) as usize..].to_vec();
        assert_eq!(recover_secret(&tail).unwrap(), secret);
        sss.push(json!({
            "name": label, "kind": "point", "threshold": threshold, "count": count,
            "entropy": entropy,
            "secret": h(secret.compress().as_bytes()),
            "shares": shares.iter().map(|s| json!({"index": s.index.0, "secret": h(s.secret.compress().as_bytes())})).collect::<Vec<_>>(),
        }));
    }
    let dup = vec![
        Share { index: Index(1), secret: Scalar::ONE },
        Share { index: Index(1), secret: Scalar::ONE },
    ];
    assert!(recover_secret(&dup).is_err());
    root.insert("shamir".into(), Value::Array(sss));

    // ---- PIN hashing (Argon2id v0x13; salt = be4(len)‖version‖be4(len)‖info) ----
    let mut pins = vec![];
    for (mode, pin, version, info) in [
        ("Standard2019", &b"1234"[..], [5u8; 16], &b"artemis"[..]),
        ("FastInsecure", &b"1234"[..], [5u8; 16], &b"artemis"[..]),
        ("FastInsecure", &b""[..], [0u8; 16], &b""[..]),
        ("FastInsecure", &b"correct horse battery staple"[..], [0xa5u8; 16], &b"stored/recovery/v1"[..]),
    ] {
        let (ak, seed) = pin_hash(mode, pin, &version, info);
        pins.push(json!({
            "mode": mode, "pin": h(pin), "version": h(&version), "user_info": h(info),
            "access_key": h(&ak), "encryption_key_seed": h(&seed),
        }));
    }
    root.insert("pin_hashing".into(), Value::Array(pins));
    root.insert(
        "argon2_params".into(),
        json!({
            "Standard2019": {"m_cost_kib": 1024 * 16, "t_cost": 32, "p_cost": 1, "output_len": 64},
            "FastInsecure": {"m_cost_kib": Params::MIN_M_COST, "t_cost": Params::MIN_T_COST, "p_cost": Params::MIN_P_COST, "output_len": 64},
        }),
    );

    // ---- one registration's derivations, for 3 realms, threshold 2 ----
    let realms = [RealmId([0x11; 16]), RealmId([0x22; 16]), RealmId([0x33; 16])];
    let (mut tape, entropy) = Tape::new("registration", 64 + 64 + 32 + 64 + 64);
    let version = [0x5au8; 16];
    let pin = b"2580";
    let info = b"opaque-subject";
    let secret_bytes = b"a K2 recovery key, opaque to every realm";
    let (access_key, enc_seed) = pin_hash("FastInsecure", pin, &version, info);
    let root_key = oprf::PrivateKey::random(&mut tape);
    let key_shares: Vec<oprf::PrivateKey> =
        create_shares(root_key.expose_secret(), 2, 3, &mut tape)
            .map(|s| oprf::PrivateKey::from(s.secret))
            .collect();
    let signing_key = OprfSigningKey::new_random(&mut tape);
    let signed: Vec<_> = key_shares
        .iter()
        .zip(&realms)
        .map(|(k, r)| sign_public_key(k.to_public_key(), r, &signing_key))
        .collect();
    let oprf_out = oprf::unoblivious_evaluate(&root_key, &access_key);
    let (unlock_key, unlock_commitment) = unlock_key_and_commitment(&oprf_out);
    let enc_scalar = Scalar::random(&mut tape);
    let enc_shares: Vec<Share<Scalar>> = create_shares(&enc_scalar, 2, 3, &mut tape).collect();
    tape.assert_empty("registration");
    let ekey = encryption_key(&enc_seed, &enc_scalar);
    let encrypted = encrypt_secret(&ekey, secret_bytes);
    let uk = UnlockKey::from(unlock_key);
    let per_realm: Vec<Value> = realms
        .iter()
        .enumerate()
        .map(|(i, r)| {
            let share = UserSecretEncryptionKeyScalarShare::from(enc_shares[i].secret);
            let tag = UnlockKeyTag::derive(&uk, r);
            let commit = EncryptedUserSecretCommitment::derive(
                &uk,
                r,
                &share,
                &EncryptedUserSecret::from(encrypted),
            );
            assert!(signed[i].verify(r).is_ok());
            json!({
                "realm_id": h(&r.0),
                "share_index": i + 1,
                "oprf_private_key_share": h(key_shares[i].expose_secret().as_bytes()),
                "oprf_signed_public_key_cbor": cbor(&signed[i]),
                "unlock_key_tag": h(tag.expose_secret()),
                "encryption_key_scalar_share": h(share.as_bytes()),
                "encrypted_secret_commitment": h(commit.expose_secret()),
            })
        })
        .collect();
    root.insert(
        "registration".into(),
        json!({
            "entropy": entropy,
            "draw_order": ["oprf root key (64)", "oprf key share coefficient (64)", "ed25519 signing key (32)", "encryption scalar (64)", "encryption share coefficient (64)"],
            "pin_hashing_mode": "FastInsecure",
            "pin": h(pin), "user_info": h(info), "version": h(&version), "secret": h(secret_bytes),
            "access_key": h(&access_key), "encryption_key_seed": h(&enc_seed),
            "oprf_root_key": h(root_key.expose_secret().as_bytes()),
            "oprf_output": h(oprf_out.expose_secret()),
            "unlock_key": h(&unlock_key), "unlock_key_commitment": h(&unlock_commitment),
            "encryption_key_scalar": h(enc_scalar.as_bytes()),
            "encryption_key": h(&ekey),
            "encrypted_secret": h(&encrypted),
            "realms": per_realm,
        }),
    );

    // ---- Noise_NK_25519_ChaChaPoly_BLAKE2s ----
    let mut noise_vectors = vec![];
    for (i, (req_payload, resp_payload)) in [
        (&b""[..], &b""[..]),
        (&b"Recover1"[..], &b"a padded response"[..]),
    ]
    .iter()
    .enumerate()
    {
        let label = format!("noise-{i}");
        let (mut tape, entropy) = Tape::new(&label, 64);
        let server_secret = x25519::StaticSecret::from([0x40 + i as u8; 32]);
        let server_public = x25519::PublicKey::from(&server_secret);
        let (client_hs, request) =
            nclient::Handshake::start(&server_public, req_payload, &mut tape).unwrap();
        let (server_hs, got_payload) =
            nserver::Handshake::start((&server_secret, &server_public), &request, &mut tape)
                .unwrap();
        tape.assert_empty(&label);
        assert_eq!(&got_payload, req_payload);
        let (mut server_t, response) = server_hs.finish(resp_payload).unwrap();
        let (mut client_t, got_resp) = client_hs.finish(&response).unwrap();
        assert_eq!(&got_resp, resp_payload);
        let mut transport = vec![];
        for (dir, msg) in [
            ("c2s", &b"Register2 under forward secrecy"[..]),
            ("s2c", &b"Ok"[..]),
            ("c2s", &b"second message, nonce 1"[..]),
            ("s2c", &b""[..]),
        ] {
            let ct = if dir == "c2s" {
                let ct = client_t.encrypt(msg).unwrap();
                assert_eq!(server_t.decrypt(&ct).unwrap(), msg);
                ct
            } else {
                let ct = server_t.encrypt(msg).unwrap();
                assert_eq!(client_t.decrypt(&ct).unwrap(), msg);
                ct
            };
            transport.push(json!({"dir": dir, "plaintext": h(msg), "ciphertext": h(&ct)}));
        }
        noise_vectors.push(json!({
            "name": label,
            "entropy": entropy,
            "draw_order": ["client ephemeral (32)", "server ephemeral (32)"],
            "server_static_secret": h(&server_secret.to_bytes()),
            "server_static_public": h(server_public.as_bytes()),
            "request_payload": h(req_payload),
            "handshake_request_cbor": cbor(&request),
            "response_payload": h(resp_payload),
            "handshake_response_cbor": cbor(&response),
            "transport": transport,
        }));
    }
    root.insert("noise".into(), Value::Array(noise_vectors));

    // ---- wire encodings (juicebox_marshalling = ciborium) ----
    let (mut tape, entropy) = Tape::new("wire", 64 * 4 + 32);
    let sk = oprf::PrivateKey::random(&mut tape);
    let pk = sk.to_public_key();
    let (_bf, bi) = oprf::start(b"wire", &mut tape);
    let (bo, proof) = oprf::blind_verifiable_evaluate(&sk, &pk, &bi, &mut tape);
    let skey = OprfSigningKey::new_random(&mut tape);
    let spk = sign_public_key(pk.clone(), &RealmId([0x77; 16]), &skey);
    let _ = Scalar::random(&mut tape);
    tape.assert_empty("wire");
    let v = RegistrationVersion::from([0x01; 16]);
    let reg2 = Register2Request {
        version: v.clone(),
        oprf_private_key: sk.clone(),
        oprf_signed_public_key: spk.clone(),
        unlock_key_commitment: UnlockKeyCommitment::from([0x02; 32]),
        unlock_key_tag: UnlockKeyTag::from([0x03; 16]),
        encryption_key_scalar_share: UserSecretEncryptionKeyScalarShare::from(Scalar::from(7u32)),
        encrypted_secret: EncryptedUserSecret::from([0x04; 145]),
        encrypted_secret_commitment: EncryptedUserSecretCommitment::from([0x05; 16]),
        policy: Policy { num_guesses: 10 },
    };
    let secrets_requests = vec![
        ("Register1", SecretsRequest::Register1),
        ("Register2", SecretsRequest::Register2(Box::new(reg2))),
        ("Recover1", SecretsRequest::Recover1),
        (
            "Recover2",
            SecretsRequest::Recover2(Recover2Request {
                version: v.clone(),
                oprf_blinded_input: bi.clone(),
            }),
        ),
        (
            "Recover3",
            SecretsRequest::Recover3(Recover3Request {
                version: v.clone(),
                unlock_key_tag: UnlockKeyTag::from([0x03; 16]),
            }),
        ),
        ("Delete", SecretsRequest::Delete),
    ];
    let secrets_responses = vec![
        ("Register1.Ok", SecretsResponse::Register1(Register1Response::Ok)),
        ("Register2.Ok", SecretsResponse::Register2(Register2Response::Ok)),
        ("Recover1.Ok", SecretsResponse::Recover1(Recover1Response::Ok { version: v.clone() })),
        ("Recover1.NotRegistered", SecretsResponse::Recover1(Recover1Response::NotRegistered)),
        ("Recover1.NoGuesses", SecretsResponse::Recover1(Recover1Response::NoGuesses)),
        (
            "Recover2.Ok",
            SecretsResponse::Recover2(Recover2Response::Ok {
                oprf_signed_public_key: spk.clone(),
                oprf_blinded_result: bo.clone(),
                oprf_proof: proof.clone(),
                unlock_key_commitment: UnlockKeyCommitment::from([0x02; 32]),
                num_guesses: 10,
                guess_count: 1,
            }),
        ),
        ("Recover2.VersionMismatch", SecretsResponse::Recover2(Recover2Response::VersionMismatch)),
        ("Recover2.NotRegistered", SecretsResponse::Recover2(Recover2Response::NotRegistered)),
        ("Recover2.NoGuesses", SecretsResponse::Recover2(Recover2Response::NoGuesses)),
        (
            "Recover3.Ok",
            SecretsResponse::Recover3(Recover3Response::Ok {
                encryption_key_scalar_share: UserSecretEncryptionKeyScalarShare::from(Scalar::from(7u32)),
                encrypted_secret: EncryptedUserSecret::from([0x04; 145]),
                encrypted_secret_commitment: EncryptedUserSecretCommitment::from([0x05; 16]),
            }),
        ),
        ("Recover3.VersionMismatch", SecretsResponse::Recover3(Recover3Response::VersionMismatch)),
        ("Recover3.NotRegistered", SecretsResponse::Recover3(Recover3Response::NotRegistered)),
        (
            "Recover3.BadUnlockKeyTag",
            SecretsResponse::Recover3(Recover3Response::BadUnlockKeyTag { guesses_remaining: 3 }),
        ),
        ("Recover3.NoGuesses", SecretsResponse::Recover3(Recover3Response::NoGuesses)),
        ("Delete.Ok", SecretsResponse::Delete(DeleteResponse::Ok)),
    ];
    let mut wire = serde_json::Map::new();
    wire.insert("entropy".into(), json!(entropy));
    wire.insert(
        "draw_order".into(),
        json!(["oprf private key (64)", "blinding factor (64)", "dleq beta_t (64)", "ed25519 signing key (32)", "unused scalar (64)"]),
    );
    wire.insert(
        "fields".into(),
        json!({
            "version": h(v.expose_secret()),
            "oprf_private_key": h(sk.expose_secret().as_bytes()),
            "oprf_public_key": h(pk.as_bytes()),
            "signed_public_key_realm": h(&[0x77u8; 16]),
            "blinded_input": cbor_bytes32(&bi),
            "blinded_output": cbor_bytes32(&bo),
            "proof_cbor": cbor(&proof),
            "signed_public_key_cbor": cbor(&spk),
        }),
    );
    wire.insert(
        "secrets_requests".into(),
        Value::Array(
            secrets_requests
                .iter()
                .map(|(n, r)| {
                    json!({"name": n, "cbor": cbor(r), "needs_forward_secrecy": r.needs_forward_secrecy()})
                })
                .collect(),
        ),
    );
    wire.insert(
        "secrets_responses".into(),
        Value::Array(
            secrets_responses
                .iter()
                .map(|(n, r)| {
                    let padded = PaddedSecretsResponse::try_from(r).unwrap();
                    json!({"name": n, "cbor": cbor(r), "padded_cbor": cbor(&padded)})
                })
                .collect(),
        ),
    );
    let hs_req = nclient::HandshakeRequest {
        client_ephemeral_public: vec![0x0e; 32],
        payload_ciphertext: vec![0x0c; 24],
    };
    let hs_resp = nclient::HandshakeResponse {
        server_ephemeral_public: vec![0x0f; 32],
        payload_ciphertext: vec![0x0d; 16],
    };
    let client_requests = vec![
        (
            "Handshake.HandshakeOnly",
            ClientRequest {
                realm: RealmId([0x77; 16]),
                auth_token: AuthToken::from("hdr.claims.sig".to_string()),
                session_id: SessionId(0xdeadbeef),
                kind: ClientRequestKind::HandshakeOnly,
                encrypted: NoiseRequest::Handshake { handshake: hs_req.clone() },
            },
        ),
        (
            "Handshake.SecretsRequest",
            ClientRequest {
                realm: RealmId([0x77; 16]),
                auth_token: AuthToken::from("hdr.claims.sig".to_string()),
                session_id: SessionId(7),
                kind: ClientRequestKind::SecretsRequest,
                encrypted: NoiseRequest::Handshake { handshake: hs_req },
            },
        ),
        (
            "Transport",
            ClientRequest {
                realm: RealmId([0x77; 16]),
                auth_token: AuthToken::from("hdr.claims.sig".to_string()),
                session_id: SessionId(70000),
                kind: ClientRequestKind::SecretsRequest,
                encrypted: NoiseRequest::Transport { ciphertext: vec![0x0a; 40] },
            },
        ),
    ];
    wire.insert(
        "client_requests".into(),
        Value::Array(
            client_requests
                .iter()
                .map(|(n, r)| json!({"name": n, "cbor": cbor(r)}))
                .collect(),
        ),
    );
    let client_responses = vec![
        (
            "Ok.Handshake",
            ClientResponse::Ok(NoiseResponse::Handshake {
                handshake: hs_resp,
                session_lifetime: Duration::from_secs(300),
            }),
        ),
        (
            "Ok.Handshake.fractional",
            ClientResponse::Ok(NoiseResponse::Handshake {
                handshake: nclient::HandshakeResponse {
                    server_ephemeral_public: vec![0x0f; 32],
                    payload_ciphertext: vec![],
                },
                session_lifetime: Duration::new(5, 250_000_000),
            }),
        ),
        ("Ok.Transport", ClientResponse::Ok(NoiseResponse::Transport { ciphertext: vec![0x0b; 20] })),
        ("Unavailable", ClientResponse::Unavailable),
        ("InvalidAuth", ClientResponse::InvalidAuth),
        ("MissingSession", ClientResponse::MissingSession),
        ("SessionError", ClientResponse::SessionError),
        ("DecodingError", ClientResponse::DecodingError),
        ("PayloadTooLarge", ClientResponse::PayloadTooLarge),
        ("RateLimitExceeded", ClientResponse::RateLimitExceeded),
    ];
    wire.insert(
        "client_responses".into(),
        Value::Array(
            client_responses
                .iter()
                .map(|(n, r)| json!({"name": n, "cbor": cbor(r)}))
                .collect(),
        ),
    );
    root.insert("wire".into(), Value::Object(wire));

    println!("{}", serde_json::to_string_pretty(&Value::Object(root)).unwrap());
}
