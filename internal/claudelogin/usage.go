package claudelogin

// Native Code queries these OAuth windows before inference. In gateway-managed
// mode they describe local availability, not the logged-in subscription. Unknown
// reset times stay null; real allowance, exhaustion and rotation stay in Magpie.
// OAuth authorization and profile requests continue to the official service.
const gatewayUsageJSON = `{"five_hour":{"utilization":0,"resets_at":null},"seven_day":{"utilization":0,"resets_at":null},"seven_day_sonnet":null,"seven_day_opus":null,"limits":[],"extra_usage":{"is_enabled":false,"monthly_limit":null,"used_credits":null,"utilization":null},"magpie_gateway_managed":true}`
