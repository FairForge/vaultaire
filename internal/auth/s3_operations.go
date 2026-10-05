package auth

// The S3 operation names — ONE list (WP-R5-12 / R5-21).
//
// The S3 parser (internal/api, S3Parser.determineOperation) assigns these
// constants and nothing else to S3Request.Operation, and ValidPermissions —
// the names a scoped key may be granted — is built from the same list. The
// two used to be separate literal lists, and the grants list lagged the
// parser by 18 operations: a key could be given GetObject but never
// GetObjectTagging or GetBucketLocation, so `aws s3 ls`-class clients that
// call them failed with a scoped key. TestS3ParserEmitsOnlyKnownOperations
// (internal/api) reads the parser's source and fails when a literal creeps
// back in.
const (
	OpListBuckets                = "ListBuckets"
	OpCreateBucket               = "CreateBucket"
	OpDeleteBucket               = "DeleteBucket"
	OpHeadBucket                 = "HeadBucket"
	OpListObjects                = "ListObjects"
	OpListObjectVersions         = "ListObjectVersions"
	OpGetBucketLocation          = "GetBucketLocation"
	OpGetBucketVersioning        = "GetBucketVersioning"
	OpPutBucketVersioning        = "PutBucketVersioning"
	OpGetBucketNotification      = "GetBucketNotification"
	OpPutBucketNotification      = "PutBucketNotification"
	OpGetBucketLogging           = "GetBucketLogging"
	OpPutBucketLogging           = "PutBucketLogging"
	OpGetBucketInventory         = "GetBucketInventory"
	OpPutBucketInventory         = "PutBucketInventory"
	OpDeleteBucketInventory      = "DeleteBucketInventory"
	OpGetBucketAcl               = "GetBucketAcl"
	OpPutBucketAcl               = "PutBucketAcl"
	OpGetObjectLockConfiguration = "GetObjectLockConfiguration"
	OpPutObjectLockConfiguration = "PutObjectLockConfiguration"
	OpGetObject                  = "GetObject"
	OpPutObject                  = "PutObject"
	OpDeleteObject               = "DeleteObject"
	OpDeleteObjects              = "DeleteObjects"
	OpHeadObject                 = "HeadObject"
	OpPostObject                 = "PostObject"
	OpRestoreObject              = "RestoreObject"
	OpGetObjectTagging           = "GetObjectTagging"
	OpPutObjectTagging           = "PutObjectTagging"
	OpDeleteObjectTagging        = "DeleteObjectTagging"
	OpGetObjectAcl               = "GetObjectAcl"
	OpPutObjectAcl               = "PutObjectAcl"
	OpGetObjectRetention         = "GetObjectRetention"
	OpPutObjectRetention         = "PutObjectRetention"
	OpGetObjectLegalHold         = "GetObjectLegalHold"
	OpPutObjectLegalHold         = "PutObjectLegalHold"
	OpInitiateMultipartUpload    = "InitiateMultipartUpload"
	OpUploadPart                 = "UploadPart"
	OpCompleteMultipartUpload    = "CompleteMultipartUpload"
	OpAbortMultipartUpload       = "AbortMultipartUpload"
	OpListMultipartUploads       = "ListMultipartUploads"
	OpListParts                  = "ListParts"
)

// S3Operations is every operation the parser can name, in the order of the
// constants above. A scoped key may be granted any of them.
var S3Operations = []string{
	OpListBuckets, OpCreateBucket, OpDeleteBucket, OpHeadBucket,
	OpListObjects, OpListObjectVersions, OpGetBucketLocation,
	OpGetBucketVersioning, OpPutBucketVersioning,
	OpGetBucketNotification, OpPutBucketNotification,
	OpGetBucketLogging, OpPutBucketLogging,
	OpGetBucketInventory, OpPutBucketInventory, OpDeleteBucketInventory,
	OpGetBucketAcl, OpPutBucketAcl,
	OpGetObjectLockConfiguration, OpPutObjectLockConfiguration,
	OpGetObject, OpPutObject, OpDeleteObject, OpDeleteObjects, OpHeadObject,
	OpPostObject, OpRestoreObject,
	OpGetObjectTagging, OpPutObjectTagging, OpDeleteObjectTagging,
	OpGetObjectAcl, OpPutObjectAcl,
	OpGetObjectRetention, OpPutObjectRetention,
	OpGetObjectLegalHold, OpPutObjectLegalHold,
	OpInitiateMultipartUpload, OpUploadPart, OpCompleteMultipartUpload,
	OpAbortMultipartUpload, OpListMultipartUploads, OpListParts,
}

// ValidPermissions is the set of names that may appear in an API key's
// permissions list: every S3 operation the parser emits, `*` for all of
// them, and the one privilege that is not an operation
// (PermBypassGovernanceRetention). Derived from S3Operations — not a second
// list to keep in step.
var ValidPermissions = func() map[string]bool {
	m := map[string]bool{"*": true, PermBypassGovernanceRetention: true}
	for _, op := range S3Operations {
		m[op] = true
	}
	return m
}()
