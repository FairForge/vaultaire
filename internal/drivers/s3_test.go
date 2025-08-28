package drivers
import (
	"os"
	"testing"
	"go.uber.org/zap"
)
func TestS3Driver_NewS3Driver(t *testing.T) {
	// Arrange
	endpoint := os.Getenv("S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("S3_ENDPOINT not set")
	}
	accessKey := os.Getenv("S3_ACCESS_KEY")
	secretKey := os.Getenv("S3_SECRET_KEY")
	if accessKey == "" || secretKey == "" {
		t.Skip("S3 credentials not set")
	}
	// Act
	driver, err := NewS3Driver(endpoint, accessKey, secretKey, "test-region", zap.NewNop())
	// Assert
	if err != nil {
		t.Fatal(err)
	}
	if driver == nil {
		t.Fatal("driver should not be nil")
	}
}
