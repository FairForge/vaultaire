package engine

import (
	"io"
	"time"
)

type Container struct {
	ID           string
	TenantID     string
	Name         string
	Type         ContainerType
	Metadata     map[string]interface{}
	Capabilities Capabilities
}

type ContainerType string

const (
	ContainerTypeObject  ContainerType = "object"
	ContainerTypeDataset ContainerType = "dataset"
	ContainerTypeCompute ContainerType = "compute"
	ContainerTypeStream  ContainerType = "stream"
)

type Artifact struct {
	ID             string
	ContainerID    string
	Key            string
	Size           int64
	ContentType    string
	ETag           string
	Epoch          int64
	Features       map[string]interface{}
	ComputeResults map[string]interface{}
}

type Operation struct {
	ID          string
	Type        OpType
	Container   string
	Key         string
	Stream      io.Reader
	Metadata    map[string]interface{}
	PolicyHints PolicyHints
}

type OpType string

const (
	OpGet     OpType = "GET"
	OpPut     OpType = "PUT"
	OpDelete  OpType = "DELETE"
	OpList    OpType = "LIST"
	OpQuery   OpType = "QUERY"
	OpCompute OpType = "COMPUTE"
)

type PolicyHints struct {
	PreferredBackends []string
	CacheStrategy     string
	ConsistencyLevel  string
	Priority          int
}

type Capabilities struct {
	Versioning    bool
	Encryption    bool
	Compression   bool
	Deduplication bool
	Compute       bool
}

type Result struct {
	Success  bool
	Data     interface{}
	Metadata map[string]interface{}
	Metrics  Metrics
}

type Metrics struct {
	Duration     time.Duration
	BytesRead    int64
	BytesWritten int64
	BackendUsed  string
	CacheHit     bool
}
