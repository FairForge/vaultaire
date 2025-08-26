package api

import (
    "bytes"
    "encoding/xml"
    "net/http/httptest"
    "os"
    "testing"
    
    "github.com/stretchr/testify/assert"
    "github.com/stretchr/testify/require"
    "github.com/gorilla/mux"
    "go.uber.org/zap"
    "github.com/FairForge/vaultaire/internal/engine"
    "github.com/FairForge/vaultaire/internal/tenant"
    "github.com/FairForge/vaultaire/internal/drivers"
)

type ListBucketResult struct {
    XMLName  xml.Name `xml:"ListBucketResult"`
    Name     string   `xml:"Name"`
    Contents []struct {
        Key          string `xml:"Key"`
        Size         int64  `xml:"Size"`
        StorageClass string `xml:"StorageClass"`
    } `xml:"Contents"`
}

func TestS3_ListObjects(t *testing.T) {
    logger := zap.NewNop()
    eng := engine.NewEngine(logger)
    
    tempDir, err := os.MkdirTemp("", "vaultaire-test-*")
    require.NoError(t, err)
    defer func() { _ = os.RemoveAll(tempDir) }()
    
    driver := drivers.NewLocalDriver(tempDir, logger)
    eng.AddDriver("local", driver)
    eng.SetPrimary("local")
    
    server := &Server{
        logger: logger,
        router: mux.NewRouter(),
        engine: eng,
    }
    server.router.PathPrefix("/").HandlerFunc(server.handleS3Request)
    
    testTenant := &tenant.Tenant{
        ID:        "test-tenant",
        Namespace: "tenant/test-tenant/",
    }
    
    // Put multiple objects first
    objects := []string{"file1.txt", "file2.txt", "dir/file3.txt"}
    for _, key := range objects {
        putReq := httptest.NewRequest("PUT", "/test-bucket/"+key, 
            bytes.NewReader([]byte("content of "+key)))
        ctx := tenant.WithTenant(putReq.Context(), testTenant)
        putReq = putReq.WithContext(ctx)
        server.handleS3Request(httptest.NewRecorder(), putReq)
    }
    
    // LIST the bucket
    listReq := httptest.NewRequest("GET", "/test-bucket", nil)
    ctx := tenant.WithTenant(listReq.Context(), testTenant)
    listReq = listReq.WithContext(ctx)
    listW := httptest.NewRecorder()
    
    server.handleS3Request(listW, listReq)
    assert.Equal(t, 200, listW.Code, "LIST should return 200")
    
    // Parse XML response
    var result ListBucketResult
    err = xml.Unmarshal(listW.Body.Bytes(), &result)
    require.NoError(t, err)
    
    assert.Equal(t, "test-bucket", result.Name)
    assert.Equal(t, 3, len(result.Contents))
}
