package recovery

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"

	"github.com/gtank/ristretto255"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testdata/sdk_transcript.json is the MIT demo client's real bytes against
// two software realms (docs/reviews/K5-PROBE.md § The lab). Knowing only the
// demo's PINs and userInfo, everything the SDK derived is re-derived here,
// and every message re-encodes to the bytes on the wire.

type transcript struct {
	Configuration struct {
		Realms []struct {
			ID string `json:"id"`
		} `json:"realms"`
	} `json:"configuration"`
	Realms []struct {
		ID        string `json:"id"`
		Exchanges []struct {
			Step     string `json:"step"`
			Status   int    `json:"status"`
			Request  hexb   `json:"request"`
			Response hexb   `json:"response"`
		} `json:"exchanges"`
	} `json:"realms"`
	Probes []struct {
		Probe    string `json:"probe"`
		Status   int    `json:"status"`
		Request  hexb   `json:"request"`
		Response hexb   `json:"response"`
	} `json:"probes"`
}

func TestRecovery_SDKTranscript_EveryDerivationAndEncoding(t *testing.T) {
	raw, err := os.ReadFile("testdata/sdk_transcript.json")
	require.NoError(t, err)
	var tr transcript
	require.NoError(t, json.Unmarshal(raw, &tr))
	require.Len(t, tr.Realms, 2)

	// Every message decodes and re-encodes to the exact bytes.
	for _, realm := range tr.Realms {
		for _, ex := range realm.Exchanges {
			req, err := UnmarshalSecretsRequest(ex.Request)
			require.NoError(t, err, ex.Step)
			again, err := req.MarshalCBOR()
			require.NoError(t, err)
			assert.Equal(t, []byte(ex.Request), again, ex.Step)
			resp, err := UnmarshalSecretsResponse(ex.Response)
			require.NoError(t, err, ex.Step)
			again, err = resp.MarshalCBOR()
			require.NoError(t, err)
			assert.Equal(t, []byte(ex.Response), again, ex.Step)
			assert.Equal(t, req.Op, resp.Op, ex.Step)
		}
	}
	for _, p := range tr.Probes {
		if p.Status != 200 {
			continue
		}
		resp, err := UnmarshalSecretsResponse(p.Response)
		require.NoError(t, err, p.Probe)
		again, _ := resp.MarshalCBOR()
		assert.Equal(t, []byte(p.Response), again, p.Probe)
	}

	// The two registrations of the demo (rust/cli/demo/src/demo.rs).
	for _, reg := range []struct {
		at                 int // the Register2 exchange
		pin, info, secret  string
		recover2, recover3 int // a successful recovery's exchanges
	}{
		{at: 1, pin: "1234", info: "presso", secret: "teyla21", recover2: 5, recover3: 6},
		{at: 13, pin: "4321", info: "teyla", secret: "presso42", recover2: 15, recover3: 16},
	} {
		var ids []RealmID
		var reqs []*Register2Request
		for _, realm := range tr.Realms { // configured in id order: share index = position + 1
			id, err := ParseRealmID(realm.ID)
			require.NoError(t, err)
			ids = append(ids, id)
			r, err := UnmarshalSecretsRequest(realm.Exchanges[reg.at].Request)
			require.NoError(t, err)
			require.Equal(t, OpRegister2, r.Op)
			reqs = append(reqs, r.Register2)
		}
		require.Less(t, hex.EncodeToString(ids[0][:]), hex.EncodeToString(ids[1][:]))
		require.Equal(t, reqs[0].Version, reqs[1].Version, "one version across realms")
		ak, seed := HashPIN(Standard2019, []byte(reg.pin), reqs[0].Version, []byte(reg.info))

		var keyShares, encShares []ScalarShare
		for i, q := range reqs {
			k, err := ScalarFromBytes(q.OPRFPrivateKey)
			require.NoError(t, err)
			keyShares = append(keyShares, ScalarShare{Index: uint32(i + 1), Secret: k})
			e, err := ScalarFromBytes(q.EncryptionKeyScalarShare)
			require.NoError(t, err)
			encShares = append(encShares, ScalarShare{Index: uint32(i + 1), Secret: e})
			// each realm's public key is its share times G, signed for that realm
			assert.Equal(t, ristretto255.NewIdentityElement().ScalarBaseMult(k).Bytes(), q.OPRFSignedPublicKey.PublicKey[:])
			assert.True(t, q.OPRFSignedPublicKey.Verify(ids[i]), "signature of realm %d", i)
			assert.False(t, q.OPRFSignedPublicKey.Verify(ids[1-i]), "a signature names its realm")
			assert.Equal(t, reqs[0].OPRFSignedPublicKey.VerifyingKey, q.OPRFSignedPublicKey.VerifyingKey, "one signing key per registration")
		}
		root, err := RecoverScalar(keyShares)
		require.NoError(t, err)
		unlockKey, commitment := UnlockKey(OPRFEvaluate(root, ak[:]))
		encScalar, err := RecoverScalar(encShares)
		require.NoError(t, err)
		ekey := EncryptionKey(seed, to32(encScalar.Bytes()))
		for i, q := range reqs {
			assert.Equal(t, commitment, q.UnlockKeyCommitment, "unlock key commitment, realm %d", i)
			assert.Equal(t, UnlockKeyTag(unlockKey, ids[i]), q.UnlockKeyTag, "tag, realm %d", i)
			assert.Equal(t, EncryptedSecretCommitment(unlockKey, ids[i], q.EncryptionKeyScalarShare, q.EncryptedSecret),
				q.EncryptedSecretCommitment, "secret commitment, realm %d", i)
			got, err := DecryptSecret(ekey, q.EncryptedSecret)
			require.NoError(t, err)
			assert.Equal(t, reg.secret, string(got))
		}

		// The successful recovery: each realm evaluated with its share and
		// proved it; the Recover3 tag is the one derived above.
		for i, realm := range tr.Realms {
			r2, err := UnmarshalSecretsRequest(realm.Exchanges[reg.recover2].Request)
			require.NoError(t, err)
			a2, err := UnmarshalSecretsResponse(realm.Exchanges[reg.recover2].Response)
			require.NoError(t, err)
			require.Equal(t, StatusOK, a2.Status)
			bi, _ := ElementFromBytes(r2.Recover2.BlindedInput)
			assert.Equal(t, OPRFBlindEvaluate(keyShares[i].Secret, bi).Bytes(), a2.Recover2.BlindedOutput[:])
			assert.True(t, VerifyDLEQ(r2.Recover2.BlindedInput, a2.Recover2.OPRFSignedPublicKey.PublicKey, a2.Recover2.BlindedOutput, a2.Recover2.Proof))
			bad := a2.Recover2.Proof
			bad.C[0] ^= 1
			assert.False(t, VerifyDLEQ(r2.Recover2.BlindedInput, a2.Recover2.OPRFSignedPublicKey.PublicKey, a2.Recover2.BlindedOutput, bad))
			r3, err := UnmarshalSecretsRequest(realm.Exchanges[reg.recover3].Request)
			require.NoError(t, err)
			assert.Equal(t, UnlockKeyTag(unlockKey, ids[i]), r3.Recover3.UnlockKeyTag)
		}
	}
}
