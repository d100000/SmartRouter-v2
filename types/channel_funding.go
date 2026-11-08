package types

// ChannelFundingAttempt freezes the non-secret identity of an upstream attempt.
// Key indices are display hints only; writes must validate BindingID and VersionID.
type ChannelFundingAttempt struct {
	BindingActivatedAt int64  `json:"binding_activated_at"`
	ChannelID          int    `json:"channel_id"`
	ChannelType        int    `json:"channel_type"`
	BindingID          string `json:"binding_id"`
	VersionID          string `json:"credential_version_id"`
	KeyIndex           int    `json:"key_index"`
	KeyAlias           string `json:"key_alias"`
	Model              string `json:"model"`
	Endpoint           string `json:"endpoint"`
	RequestID          string `json:"request_id"`
	StartedAt          int64  `json:"started_at"`
	Source             string `json:"source"`
}
