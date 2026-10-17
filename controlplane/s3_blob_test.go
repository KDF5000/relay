package controlplane

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Observe the buffer supplied to an unknown-length reader, not just the size
// of HTTP parts: the old SDK defaults allocated 528 MiB before the first read.
type boundedUploadReader struct {
	io.Reader
	maxRead int
}

func (r *boundedUploadReader) Read(p []byte) (int, error) {
	if len(p) > r.maxRead {
		r.maxRead = len(p)
	}
	if len(p) > 8<<20 {
		return 0, fmt.Errorf("upload requested a %d-byte buffer", len(p))
	}
	return r.Reader.Read(p)
}

func TestS3UploadUsesBoundedParts(t *testing.T) {
	for _, size := range []int{0, 1234, 8 << 20, (8 << 20) + 137} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			payload := bytes.Repeat([]byte("x"), size)
			var uploaded bytes.Buffer
			var parts []int
			completed := false
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/artifacts/test/report.txt" {
					t.Errorf("unexpected object path: %s", r.URL.Path)
					http.Error(w, "unexpected path", 400)
					return
				}
				q := r.URL.Query()
				switch {
				case r.Method == http.MethodPost && q.Has("uploads"):
					fmt.Fprint(w, `<InitiateMultipartUploadResult><Bucket>artifacts</Bucket><Key>test/report.txt</Key><UploadId>upload-1</UploadId></InitiateMultipartUploadResult>`)
				case r.Method == http.MethodPut && q.Get("uploadId") == "upload-1":
					var reader io.Reader = r.Body
					if strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") {
						reader = httputil.NewChunkedReader(reader)
					}
					part, err := io.ReadAll(reader)
					if err != nil {
						t.Errorf("read part: %v", err)
					}
					if q.Get("partNumber") != strconv.Itoa(len(parts)+1) {
						t.Errorf("out of order part: %s", r.URL.RawQuery)
					}
					parts = append(parts, len(part))
					uploaded.Write(part)
					w.Header().Set("ETag", `"part-etag"`)
				case r.Method == http.MethodPost && q.Get("uploadId") == "upload-1":
					var request struct {
						Parts []struct{ PartNumber int } `xml:"Part"`
					}
					if err := xml.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Parts) != len(parts) {
						t.Errorf("completion parts=%v err=%v", request.Parts, err)
					}
					completed = true
					fmt.Fprint(w, `<CompleteMultipartUploadResult><Bucket>artifacts</Bucket><Key>test/report.txt</Key><ETag>complete-etag</ETag></CompleteMultipartUploadResult>`)
				case r.Method == http.MethodDelete:
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected S3 request: %s %s", r.Method, r.URL)
					http.Error(w, "unexpected request", 400)
				}
			}))
			defer server.Close()
			client, err := minio.New(strings.TrimPrefix(server.URL, "https://"), &minio.Options{
				Creds:  credentials.NewStaticV4("test-access", "test-secret", ""),
				Secure: true, Region: "us-east-1", Transport: server.Client().Transport,
			})
			if err != nil {
				t.Fatal(err)
			}
			store := &S3BlobStore{client: client, bucket: "artifacts", prefix: "test"}
			reader := &boundedUploadReader{Reader: bytes.NewReader(payload)}
			n, err := store.Put(context.Background(), "report.txt", reader)
			if err != nil {
				t.Fatal(err)
			}
			if n != int64(size) || !completed || !bytes.Equal(payload, uploaded.Bytes()) {
				t.Fatalf("size=%d uploaded=%d completed=%v", n, uploaded.Len(), completed)
			}
			wantParts := []int{size}
			if size > 8<<20 {
				wantParts = []int{8 << 20, size - (8 << 20)}
			}
			if !reflect.DeepEqual(parts, wantParts) || reader.maxRead > 8<<20 {
				t.Fatalf("parts=%v want=%v buffer=%d", parts, wantParts, reader.maxRead)
			}
		})
	}
}
