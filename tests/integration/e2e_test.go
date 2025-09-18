package integration

import (
	"github.com/stretchr/testify/suite"
	"os"
	"testing"
)

type E2ETestSuite struct {
	suite.Suite
	serverURL string
	apiKey    string
	apiSecret string
}

func (suite *E2ETestSuite) SetupSuite() {
	suite.serverURL = os.Getenv("TEST_SERVER_URL")
	if suite.serverURL == "" {
		suite.serverURL = "http://localhost:8000"
	}
	suite.apiKey = "test-key"
	suite.apiSecret = "test-secret"
}

func (suite *E2ETestSuite) TestUserJourney() {
	// 1. Create bucket
	// 2. Upload file
	// 3. List files
	// 4. Download file
	// 5. Delete file
	// 6. Delete bucket
}

func TestE2E(t *testing.T) {
	suite.Run(t, new(E2ETestSuite))
}
