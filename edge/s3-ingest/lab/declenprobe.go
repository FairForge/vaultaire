//go:build ignore

// declenprobe answers RP72-2 (docs/reviews/WP-EDGE-2C-REVIEW.md): does a
// backend enforce x-amz-decoded-content-length on a HEADER-signed
// STREAMING-UNSIGNED-PAYLOAD-TRAILER write, the shape the mover forwards
// (D-EDGE-30)? For each backend registered from the environment (iDrive
// from IDRIVE_*, Lyve from LYVE_*), it signs with the DRIVER's own SignPut /
// SignUploadPart (the exact code path the admit uses) a request declaring
// the body's decoded length exactly, one byte more and one byte less, sends
// a correctly framed body with a correct CRC64NVME trailer of the bytes it
// actually sends, and records the answer; a stored object is HEADed (its
// size against the declared one) and deleted, a part's upload aborted.
// Keys under t-_lab/p47-declen/; nothing is left behind. No secret is
// printed.
//
//	set -a; . /opt/vaultaire/configs/.env; set +a; ./declenprobe
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc64"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/FairForge/vaultaire/internal/common"
	"github.com/FairForge/vaultaire/internal/drivers"
	"github.com/FairForge/vaultaire/internal/engine"
)

const trailer = "x-amz-checksum-crc64nvme"

// frame is data as aws-chunked (1 MiB chunks) with its CRC64NVME trailer.
func frame(data []byte) []byte {
	sum := make([]byte, 8)
	binary.BigEndian.PutUint64(sum, crc64.Checksum(data, crc64.MakeTable(0x9a6c9329ac4bc9b5)))
	var b bytes.Buffer
	for o := 0; o < len(data); o += 1 << 20 {
		n := min(1<<20, len(data)-o)
		fmt.Fprintf(&b, "%x\r\n", n)
		b.Write(data[o : o+n])
		b.WriteString("\r\n")
	}
	fmt.Fprintf(&b, "0\r\n%s:%s\r\n\r\n", trailer, base64.StdEncoding.EncodeToString(sum))
	return b.Bytes()
}

type writer interface {
	engine.EdgeWriter
	engine.ObjectHeader
	Delete(ctx context.Context, container, artifact string) error
	List(ctx context.Context, container, prefix string) ([]string, error)
}

func send(ctx context.Context, headers map[string]string, u string, body []byte) (int, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, io.NopCloser(bytes.NewReader(body)))
	if err != nil {
		return 0, err.Error()
	}
	for k, v := range headers {
		if k == "host" {
			continue
		}
		req.Header.Set(k, v)
	}
	req.ContentLength = -1 // Transfer-Encoding: chunked, as aws-cli sends it
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	code := ""
	if i := strings.Index(string(b), "<Code>"); i >= 0 {
		if j := strings.Index(string(b)[i:], "</Code>"); j > 0 {
			code = string(b)[i+6 : i+j]
		}
	}
	return resp.StatusCode, code
}

func probe(name string, w writer) {
	ctx := common.WithTenantID(context.Background(), "_lab")
	container := "p47-declen"
	if len(os.Args) > 1 && os.Args[1] == "-list" {
		left, err := w.List(ctx, container, "")
		fmt.Printf("%s left under t-_lab/%s/: %d %v\n", name, container, len(left), err)
		for _, k := range left {
			_ = w.Delete(ctx, container, k)
		}
		return
	}
	body := make([]byte, 2<<20+12345)
	for i := range body {
		body[i] = byte(i*131 + i>>9)
	}
	framed := frame(body)
	actual := int64(len(body))
	for _, c := range []struct {
		label string
		delta int64
	}{{"exact", 0}, {"+1", 1}, {"-1", -1}} {
		declared := actual + c.delta
		key := fmt.Sprintf("put-%s-%d", strings.TrimPrefix(c.label, "+"), time.Now().UnixNano())
		h, u, err := w.SignPut(ctx, container, key, declared, trailer)
		if err != nil {
			fmt.Printf("%s PutObject %s: sign error %v\n", name, c.label, err)
			continue
		}
		status, code := send(ctx, h, u, framed)
		line := fmt.Sprintf("%s PutObject decoded=%s (declared %d, sent %d): %d %s", name, c.label, declared, actual, status, code)
		if status == http.StatusOK {
			info, herr := w.HeadObject(ctx, container, key)
			if herr != nil {
				line += fmt.Sprintf(" — HEAD error %v", herr)
			} else {
				line += fmt.Sprintf(" — stored %d bytes (declared %d)", info.Size, declared)
			}
		}
		_ = w.Delete(ctx, container, key)
		fmt.Println(line)

		pkey := "part-" + key
		uid, err := w.CreateMultipart(ctx, container, pkey, "")
		if err != nil {
			fmt.Printf("%s UploadPart %s: create error %v\n", name, c.label, err)
			continue
		}
		ph, pu, err := w.SignUploadPart(ctx, container, pkey, uid, 1, declared, trailer)
		if err != nil {
			fmt.Printf("%s UploadPart %s: sign error %v\n", name, c.label, err)
		} else {
			status, code = send(ctx, ph, pu, framed)
			fmt.Printf("%s UploadPart decoded=%s (declared %d, sent %d): %d %s\n", name, c.label, declared, actual, status, code)
		}
		if err := w.AbortMultipart(ctx, container, pkey, uid); err != nil {
			fmt.Printf("%s UploadPart %s: ABORT FAILED %v — upload %s on %s/%s\n", name, c.label, err, uid, container, pkey)
		}
	}
}

func main() {
	logger := zap.NewNop()
	if ak := os.Getenv("IDRIVE_ACCESS_KEY"); ak != "" {
		region := drivers.IDriveDefaultRegion(os.Getenv)
		endpoint := os.Getenv("IDRIVE_ENDPOINT")
		if endpoint == "" {
			endpoint = drivers.IDriveRegionEndpoint(os.Getenv, region)
		}
		d, err := drivers.NewIDriveDriver(ak, os.Getenv("IDRIVE_SECRET_KEY"), endpoint, region, logger)
		if err != nil {
			fmt.Println("idrive:", err)
		} else {
			probe("idrive "+region, d)
		}
	}
	if ak := os.Getenv("LYVE_ACCESS_KEY"); ak != "" {
		region := os.Getenv("LYVE_REGION")
		d, err := drivers.NewLyveDriver(ak, os.Getenv("LYVE_SECRET_KEY"), "", region, logger)
		if err != nil {
			fmt.Println("lyve:", err)
		} else {
			probe("lyve "+region, d)
		}
	}
}
