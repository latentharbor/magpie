package provider
import ("net/http"; "github.com/yetone/magpie/internal/claudeteam")
func claudeTeamResponseError(method string,res *http.Response)error { return claudeteam.ResponseError(method,res) }
