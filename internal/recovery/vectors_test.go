package recovery

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"testing"

	"github.com/gtank/ristretto255"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The golden vectors of docs/reviews/K5-PROBE.md § Golden vectors:
// testdata/vectors.json, written by testdata/gen from the MIT juicebox-sdk
// crates at 2e0f544e. Every value must come out byte for byte.

type hexb []byte

func (h *hexb) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := hex.DecodeString(s)
	*h = v
	return err
}

func h32(t *testing.T, b hexb) [32]byte {
	t.Helper()
	require.Len(t, []byte(b), 32)
	return [32]byte(b)
}

type vectors struct {
	OPRF []struct {
		Name                                                string
		Entropy, Input, PrivateKey, PublicKey, BlindedInput hexb
		BlindedOutput, ProofCBOR, Output                    hexb
	}
	Shamir []struct {
		Name, Kind       string
		Threshold, Count int
		Entropy, Secret  hexb
		Shares           []struct {
			Index  uint32
			Secret hexb
		}
	}
	PinHashing []struct {
		Mode                                                 string
		Pin, Version, UserInfo, AccessKey, EncryptionKeySeed hexb
	}
	Registration struct {
		Entropy, Pin, UserInfo, Version, Secret, AccessKey, EncryptionKeySeed hexb
		OPRFRootKey, OPRFOutput, UnlockKey, UnlockKeyCommitment               hexb
		EncryptionKeyScalar, EncryptionKey, EncryptedSecret                   hexb
		Realms                                                                []struct {
			RealmID                                                    hexb
			ShareIndex                                                 uint32
			OPRFPrivateKeyShare, OPRFSignedPublicKeyCBOR, UnlockKeyTag hexb
			EncryptionKeyScalarShare, EncryptedSecretCommitment        hexb
		}
	}
	Noise []struct {
		Name                                                            string
		Entropy, ServerStaticSecret, ServerStaticPublic, RequestPayload hexb
		HandshakeRequestCBOR, ResponsePayload, HandshakeResponseCBOR    hexb
		Transport                                                       []struct {
			Dir                   string
			Plaintext, Ciphertext hexb
		}
	}
	Wire struct {
		Entropy hexb
		Fields  struct {
			Version, OPRFPrivateKey, OPRFPublicKey, SignedPublicKeyRealm hexb
			BlindedInput, BlindedOutput, ProofCBOR, SignedPublicKeyCBOR  hexb
		}
		SecretsRequests []struct {
			Name                string
			CBOR                hexb
			NeedsForwardSecrecy bool
		}
		SecretsResponses []struct {
			Name             string
			CBOR, PaddedCBOR hexb
		}
		ClientRequests, ClientResponses []struct {
			Name string
			CBOR hexb
		}
	}
}

// The generator writes snake_case keys; map them onto the fields above.
func loadVectors(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/vectors.json")
	require.NoError(t, err)
	var generic any
	require.NoError(t, json.Unmarshal(raw, &generic))
	camel, err := json.Marshal(camelKeys(generic))
	require.NoError(t, err)
	var v vectors
	require.NoError(t, json.Unmarshal(camel, &v))
	require.NotEmpty(t, v.OPRF)
	return v
}

func camelKeys(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[camel(k)] = camelKeys(val)
		}
		return out
	case []any:
		for i := range x {
			x[i] = camelKeys(x[i])
		}
		return x
	}
	return v
}

// camel turns oprf_private_key_share into OPRFPrivateKeyShare (json matches
// field names case-insensitively, so only the underscores matter).
func camel(s string) string {
	var b []byte
	up := true
	for i := 0; i < len(s); i++ {
		if s[i] == '_' {
			up = true
			continue
		}
		c := s[i]
		if up && c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		up = false
		b = append(b, c)
	}
	return string(b)
}

// tape replays a vector's recorded entropy; the test asserts it is spent.
func tape(b hexb) *bytes.Reader { return bytes.NewReader(b) }

func TestRecovery_GoldenVectors_MatchJuiceboxSDK(t *testing.T) {
	v := loadVectors(t)

	t.Run("oprf+dleq", func(t *testing.T) {
		for _, tc := range v.OPRF {
			r := tape(tc.Entropy)
			sk, err := RandomScalar(r)
			require.NoError(t, err)
			assert.Equal(t, []byte(tc.PrivateKey), sk.Bytes(), tc.Name)
			pk := to32(ristretto255.NewIdentityElement().ScalarBaseMult(sk).Bytes())
			assert.Equal(t, []byte(tc.PublicKey), pk[:], tc.Name)
			blind, bi, err := OPRFBlind(r, tc.Input)
			require.NoError(t, err)
			assert.Equal(t, []byte(tc.BlindedInput), bi[:], tc.Name)
			biE, err := ElementFromBytes(bi)
			require.NoError(t, err)
			bo := OPRFBlindEvaluate(sk, biE)
			assert.Equal(t, []byte(tc.BlindedOutput), bo.Bytes(), tc.Name)
			proof, err := ProveDLEQ(r, sk, biE, pk, bo)
			require.NoError(t, err)
			assert.Equal(t, []byte(tc.ProofCBOR), appendProof(nil, proof), tc.Name)
			assert.Zero(t, r.Len(), "%s: every recorded draw consumed, in order", tc.Name)
			assert.True(t, VerifyDLEQ(bi, pk, to32(bo.Bytes()), proof), tc.Name)
			out := OPRFFinalize(tc.Input, blind, bo)
			assert.Equal(t, []byte(tc.Output), out[:], tc.Name)
			unobl := OPRFEvaluate(sk, tc.Input)
			assert.Equal(t, []byte(tc.Output), unobl[:], tc.Name)
		}
	})

	t.Run("shamir", func(t *testing.T) {
		for _, tc := range v.Shamir {
			r := tape(tc.Entropy)
			switch tc.Kind {
			case "scalar":
				secret, err := RandomScalar(r)
				require.NoError(t, err)
				assert.Equal(t, []byte(tc.Secret), secret.Bytes(), tc.Name)
				shares, err := ShareScalar(r, secret, tc.Threshold, tc.Count)
				require.NoError(t, err)
				require.Len(t, shares, len(tc.Shares))
				for i, s := range shares {
					assert.Equal(t, tc.Shares[i].Index, s.Index, tc.Name)
					assert.Equal(t, []byte(tc.Shares[i].Secret), s.Secret.Bytes(), tc.Name)
				}
				got, err := RecoverScalar(shares[len(shares)-tc.Threshold:])
				require.NoError(t, err)
				assert.Equal(t, 1, got.Equal(secret), tc.Name)
			case "point":
				secret, err := randomElement(r)
				require.NoError(t, err)
				assert.Equal(t, []byte(tc.Secret), secret.Bytes(), tc.Name)
				shares, err := ShareElement(r, secret, tc.Threshold, tc.Count)
				require.NoError(t, err)
				for i, s := range shares {
					assert.Equal(t, []byte(tc.Shares[i].Secret), s.Secret.Bytes(), tc.Name)
				}
				got, err := RecoverElement(shares[:tc.Threshold])
				require.NoError(t, err)
				assert.Equal(t, 1, got.Equal(secret), tc.Name)
			}
			assert.Zero(t, r.Len(), tc.Name)
		}
		_, err := RecoverScalar([]ScalarShare{{1, scalarFromUint32(1)}, {1, scalarFromUint32(1)}})
		assert.ErrorIs(t, err, errDuplicateShares)
	})

	t.Run("pin hashing", func(t *testing.T) {
		for _, tc := range v.PinHashing {
			mode := FastInsecure
			if tc.Mode == "Standard2019" {
				mode = Standard2019
			}
			ak, seed := HashPIN(mode, tc.Pin, [16]byte(tc.Version), tc.UserInfo)
			assert.Equal(t, []byte(tc.AccessKey), ak[:], tc.Mode)
			assert.Equal(t, []byte(tc.EncryptionKeySeed), seed[:], tc.Mode)
		}
	})

	t.Run("registration", func(t *testing.T) {
		reg := v.Registration
		ak, seed := HashPIN(FastInsecure, reg.Pin, [16]byte(reg.Version), reg.UserInfo)
		assert.Equal(t, []byte(reg.AccessKey), ak[:])
		assert.Equal(t, []byte(reg.EncryptionKeySeed), seed[:])
		r := tape(reg.Entropy)
		root, err := RandomScalar(r)
		require.NoError(t, err)
		assert.Equal(t, []byte(reg.OPRFRootKey), root.Bytes())
		keyShares, err := ShareScalar(r, root, 2, 3)
		require.NoError(t, err)
		var seedBytes [32]byte
		_, err = io.ReadFull(r, seedBytes[:])
		require.NoError(t, err)
		signing := ed25519.NewKeyFromSeed(seedBytes[:])
		out := OPRFEvaluate(root, ak[:])
		assert.Equal(t, []byte(reg.OPRFOutput), out[:])
		unlockKey, commitment := UnlockKey(out)
		assert.Equal(t, []byte(reg.UnlockKey), unlockKey[:])
		assert.Equal(t, []byte(reg.UnlockKeyCommitment), commitment[:])
		encScalar, err := RandomScalar(r)
		require.NoError(t, err)
		assert.Equal(t, []byte(reg.EncryptionKeyScalar), encScalar.Bytes())
		encShares, err := ShareScalar(r, encScalar, 2, 3)
		require.NoError(t, err)
		assert.Zero(t, r.Len())
		ekey := EncryptionKey(seed, to32(encScalar.Bytes()))
		assert.Equal(t, []byte(reg.EncryptionKey), ekey[:])
		enc, err := EncryptSecret(ekey, reg.Secret)
		require.NoError(t, err)
		assert.Equal(t, []byte(reg.EncryptedSecret), enc[:])
		for i, realm := range reg.Realms {
			id := RealmID(realm.RealmID)
			assert.Equal(t, realm.ShareIndex, keyShares[i].Index)
			assert.Equal(t, []byte(realm.OPRFPrivateKeyShare), keyShares[i].Secret.Bytes())
			pub := to32(ristretto255.NewIdentityElement().ScalarBaseMult(keyShares[i].Secret).Bytes())
			signed := SignPublicKey(signing, id, pub)
			assert.Equal(t, []byte(realm.OPRFSignedPublicKeyCBOR), appendSigned(nil, signed), "signed public key, realm %d", i)
			assert.True(t, signed.Verify(id))
			assert.False(t, signed.Verify(RealmID{}), "the signature binds the realm id")
			tag := UnlockKeyTag(unlockKey, id)
			assert.Equal(t, []byte(realm.UnlockKeyTag), tag[:])
			share := to32(encShares[i].Secret.Bytes())
			assert.Equal(t, []byte(realm.EncryptionKeyScalarShare), share[:])
			c := EncryptedSecretCommitment(unlockKey, id, share, enc)
			assert.Equal(t, []byte(realm.EncryptedSecretCommitment), c[:])
		}
		secret, err := DecryptSecret(ekey, enc)
		require.NoError(t, err)
		assert.Equal(t, []byte(reg.Secret), secret)
	})

	t.Run("noise", func(t *testing.T) {
		for _, tc := range v.Noise {
			r := tape(tc.Entropy)
			static := h32(t, tc.ServerStaticSecret)
			pub, err := NoisePublicKey(static)
			require.NoError(t, err)
			assert.Equal(t, []byte(tc.ServerStaticPublic), pub[:])
			client, req, err := NoiseClientStart(r, pub, tc.RequestPayload)
			require.NoError(t, err)
			assert.Equal(t, []byte(tc.HandshakeRequestCBOR), appendHandshakeRequest(nil, req), tc.Name)
			server, got, err := NoiseServerStart(r, static, pub, req)
			require.NoError(t, err)
			assert.Zero(t, r.Len(), tc.Name)
			assert.Equal(t, []byte(tc.RequestPayload), append([]byte{}, got...))
			st, resp, err := server.Finish(tc.ResponsePayload)
			require.NoError(t, err)
			assert.Equal(t, []byte(tc.HandshakeResponseCBOR), appendHandshakeResponse(nil, resp), tc.Name)
			ct, gotResp, err := client.Finish(resp)
			require.NoError(t, err)
			assert.Equal(t, []byte(tc.ResponsePayload), append([]byte{}, gotResp...))
			for _, m := range tc.Transport {
				from, to := ct, st
				if m.Dir == "s2c" {
					from, to = st, ct
				}
				c, err := from.Encrypt(m.Plaintext)
				require.NoError(t, err)
				assert.Equal(t, []byte(m.Ciphertext), c, "%s %s", tc.Name, m.Dir)
				p, err := to.Decrypt(c)
				require.NoError(t, err)
				assert.Equal(t, []byte(m.Plaintext), append([]byte{}, p...))
			}
		}
	})

	t.Run("wire", func(t *testing.T) {
		w := v.Wire
		f := w.Fields
		var proof Proof
		{
			it, err := decodeCBOR(f.ProofCBOR)
			require.NoError(t, err)
			pf, err := it.fields("c", "beta_z")
			require.NoError(t, err)
			require.NoError(t, scalarField(pf["c"], &proof.C))
			require.NoError(t, scalarField(pf["beta_z"], &proof.BetaZ))
		}
		// the field values are themselves reproducible from the tape
		r := tape(w.Entropy)
		sk, err := RandomScalar(r)
		require.NoError(t, err)
		assert.Equal(t, []byte(f.OPRFPrivateKey), sk.Bytes())
		_, bi, err := OPRFBlind(r, []byte("wire"))
		require.NoError(t, err)
		assert.Equal(t, []byte(f.BlindedInput), bi[:])
		biE, _ := ElementFromBytes(bi)
		bo := OPRFBlindEvaluate(sk, biE)
		pk := to32(ristretto255.NewIdentityElement().ScalarBaseMult(sk).Bytes())
		p2, err := ProveDLEQ(r, sk, biE, pk, bo)
		require.NoError(t, err)
		assert.Equal(t, proof, p2)
		var seedBytes [32]byte
		_, _ = io.ReadFull(r, seedBytes[:])
		signed := SignPublicKey(ed25519.NewKeyFromSeed(seedBytes[:]), RealmID(f.SignedPublicKeyRealm), pk)
		assert.Equal(t, []byte(f.SignedPublicKeyCBOR), appendSigned(nil, signed))

		version := [16]byte(f.Version)
		seven := to32(scalarFromUint32(7).Bytes())
		var enc [EncryptedSecretSize]byte
		copy(enc[:], bytes.Repeat([]byte{4}, EncryptedSecretSize))
		reg2 := &Register2Request{
			Version: version, OPRFPrivateKey: to32(sk.Bytes()), OPRFSignedPublicKey: signed,
			UnlockKeyCommitment: [32]byte(bytes.Repeat([]byte{2}, 32)), UnlockKeyTag: [16]byte(bytes.Repeat([]byte{3}, 16)),
			EncryptionKeyScalarShare: seven, EncryptedSecret: enc,
			EncryptedSecretCommitment: [16]byte(bytes.Repeat([]byte{5}, 16)), Policy: Policy{NumGuesses: 10},
		}
		requests := map[string]SecretsRequest{
			"Register1": {Op: OpRegister1},
			"Register2": {Op: OpRegister2, Register2: reg2},
			"Recover1":  {Op: OpRecover1},
			"Recover2":  {Op: OpRecover2, Recover2: &Recover2Request{Version: version, BlindedInput: bi}},
			"Recover3":  {Op: OpRecover3, Recover3: &Recover3Request{Version: version, UnlockKeyTag: reg2.UnlockKeyTag}},
			"Delete":    {Op: OpDelete},
		}
		for _, tc := range w.SecretsRequests {
			req, ok := requests[tc.Name]
			require.True(t, ok, tc.Name)
			got, err := req.MarshalCBOR()
			require.NoError(t, err)
			assert.Equal(t, []byte(tc.CBOR), got, tc.Name)
			assert.Equal(t, tc.NeedsForwardSecrecy, req.NeedsForwardSecrecy(), tc.Name)
			back, err := UnmarshalSecretsRequest(tc.CBOR)
			require.NoError(t, err, tc.Name)
			assert.Equal(t, req, back, tc.Name)
		}

		r2 := &Recover2OK{OPRFSignedPublicKey: signed, BlindedOutput: to32(bo.Bytes()), Proof: proof,
			UnlockKeyCommitment: reg2.UnlockKeyCommitment, NumGuesses: 10, GuessCount: 1}
		r3 := &Recover3OK{EncryptionKeyScalarShare: seven, EncryptedSecret: enc, EncryptedSecretCommitment: reg2.EncryptedSecretCommitment}
		responses := map[string]SecretsResponse{
			"Register1.Ok": {Op: OpRegister1}, "Register2.Ok": {Op: OpRegister2},
			"Recover1.Ok": {Op: OpRecover1, Version: version}, "Recover1.NotRegistered": {Op: OpRecover1, Status: StatusNotRegistered},
			"Recover1.NoGuesses": {Op: OpRecover1, Status: StatusNoGuesses},
			"Recover2.Ok":        {Op: OpRecover2, Recover2: r2}, "Recover2.VersionMismatch": {Op: OpRecover2, Status: StatusVersionMismatch},
			"Recover2.NotRegistered": {Op: OpRecover2, Status: StatusNotRegistered}, "Recover2.NoGuesses": {Op: OpRecover2, Status: StatusNoGuesses},
			"Recover3.Ok": {Op: OpRecover3, Recover3: r3}, "Recover3.VersionMismatch": {Op: OpRecover3, Status: StatusVersionMismatch},
			"Recover3.NotRegistered":   {Op: OpRecover3, Status: StatusNotRegistered},
			"Recover3.BadUnlockKeyTag": {Op: OpRecover3, Status: StatusBadUnlockKeyTag, GuessesRemaining: 3},
			"Recover3.NoGuesses":       {Op: OpRecover3, Status: StatusNoGuesses}, "Delete.Ok": {Op: OpDelete},
		}
		require.Len(t, w.SecretsResponses, len(responses))
		for _, tc := range w.SecretsResponses {
			resp, ok := responses[tc.Name]
			require.True(t, ok, tc.Name)
			got, err := resp.MarshalCBOR()
			require.NoError(t, err)
			assert.Equal(t, []byte(tc.CBOR), got, tc.Name)
			padded, err := resp.MarshalPadded()
			require.NoError(t, err)
			assert.Equal(t, []byte(tc.PaddedCBOR), padded, tc.Name)
			back, err := UnmarshalSecretsResponse(tc.CBOR)
			require.NoError(t, err, tc.Name)
			assert.Equal(t, resp, back, tc.Name)
			back, err = UnmarshalPadded(tc.PaddedCBOR)
			require.NoError(t, err, tc.Name)
			assert.Equal(t, resp, back, tc.Name)
		}

		realm := RealmID(bytes.Repeat([]byte{0x77}, 16))
		hsReq := &HandshakeRequest{ClientEphemeralPublic: bytes.Repeat([]byte{0x0e}, 32), PayloadCiphertext: bytes.Repeat([]byte{0x0c}, 24)}
		creqs := map[string]ClientRequest{
			"Handshake.HandshakeOnly":  {Realm: realm, AuthToken: "hdr.claims.sig", SessionID: 0xdeadbeef, Kind: KindHandshakeOnly, Handshake: hsReq},
			"Handshake.SecretsRequest": {Realm: realm, AuthToken: "hdr.claims.sig", SessionID: 7, Kind: KindSecretsRequest, Handshake: hsReq},
			"Transport":                {Realm: realm, AuthToken: "hdr.claims.sig", SessionID: 70000, Kind: KindSecretsRequest, Transport: bytes.Repeat([]byte{0x0a}, 40)},
		}
		for _, tc := range w.ClientRequests {
			req := creqs[tc.Name]
			got, err := req.MarshalCBOR()
			require.NoError(t, err)
			assert.Equal(t, []byte(tc.CBOR), got, tc.Name)
			back, err := UnmarshalClientRequest(tc.CBOR)
			require.NoError(t, err)
			assert.Equal(t, req, back, tc.Name)
		}
		hsResp := &HandshakeResponse{ServerEphemeralPublic: bytes.Repeat([]byte{0x0f}, 32), PayloadCiphertext: bytes.Repeat([]byte{0x0d}, 16)}
		cresps := map[string]ClientResponse{
			"Ok.Handshake":            {Handshake: hsResp, SessionLifetime: 300e9},
			"Ok.Handshake.fractional": {Handshake: &HandshakeResponse{ServerEphemeralPublic: hsResp.ServerEphemeralPublic, PayloadCiphertext: []byte{}}, SessionLifetime: 5250e6},
			"Ok.Transport":            {Transport: bytes.Repeat([]byte{0x0b}, 20)},
			"Unavailable":             {Status: ClientUnavailable}, "InvalidAuth": {Status: ClientInvalidAuth},
			"MissingSession": {Status: ClientMissingSession}, "SessionError": {Status: ClientSessionError},
			"DecodingError": {Status: ClientDecodingError}, "PayloadTooLarge": {Status: ClientPayloadTooLarge},
			"RateLimitExceeded": {Status: ClientRateLimitExceeded},
		}
		require.Len(t, w.ClientResponses, len(cresps))
		for _, tc := range w.ClientResponses {
			resp := cresps[tc.Name]
			got, err := resp.MarshalCBOR()
			require.NoError(t, err)
			assert.Equal(t, []byte(tc.CBOR), got, tc.Name)
			back, err := UnmarshalClientResponse(tc.CBOR)
			require.NoError(t, err)
			assert.Equal(t, resp, back, tc.Name)
		}
	})
}
