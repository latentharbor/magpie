package provider

import (
	"github.com/yetone/magpie/internal/claudeteam"
	"net/http"
)

func claudeTeamResponseError(method string, res *http.Response) error {
	return claudeteam.ResponseError(method, res)
}
