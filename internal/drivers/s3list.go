package drivers

import "github.com/aws/aws-sdk-go-v2/service/s3"

// s3ListPaginator is the one way drivers walk ListObjectsV2 pages. It stops
// on a repeated continuation token: a gateway that keeps answering
// IsTruncated=true with the same token (a hostile or broken backend) would
// otherwise spin the caller forever — the SDK default is to trust the token
// (Review R7-06 adversarial pass).
func s3ListPaginator(client s3.ListObjectsV2APIClient, in *s3.ListObjectsV2Input) *s3.ListObjectsV2Paginator {
	return s3.NewListObjectsV2Paginator(client, in, func(o *s3.ListObjectsV2PaginatorOptions) {
		o.StopOnDuplicateToken = true
	})
}
