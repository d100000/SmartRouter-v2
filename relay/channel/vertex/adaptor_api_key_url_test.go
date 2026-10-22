package vertex

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetRequestURLWithAPIKey(t *testing.T) {
	tests := []struct {
		name      string
		projectID string
		region    string
		stream    bool
		want      string
	}{
		{
			name:      "project ID keeps the global region",
			projectID: "my-project",
			region:    `{"default":"global"}`,
			want:      "https://aiplatform.googleapis.com/v1/projects/my-project/locations/global/publishers/google/models/gemini-4-argon:generateContent?key=test-key",
		},
		{
			name:      "project ID keeps a regional endpoint when streaming",
			projectID: "my-project",
			region:    `{"default":"us-central1"}`,
			stream:    true,
			want:      "https://us-central1-aiplatform.googleapis.com/v1/projects/my-project/locations/us-central1/publishers/google/models/gemini-4-argon:streamGenerateContent?alt=sse&key=test-key",
		},
		{
			name:   "no project ID keeps the express-mode URL",
			region: `{"default":"global"}`,
			want:   "https://aiplatform.googleapis.com/v1/publishers/google/models/gemini-4-argon:generateContent?key=test-key",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := &relaycommon.RelayInfo{
				IsStream:        tt.stream,
				OriginModelName: "gemini-4-argon",
				ChannelMeta: &relaycommon.ChannelMeta{
					ApiVersion:        tt.region,
					ApiKey:            "test-key",
					UpstreamModelName: "gemini-4-argon",
					ChannelOtherSettings: dto.ChannelOtherSettings{
						VertexKeyType:   dto.VertexKeyTypeAPIKey,
						VertexProjectID: tt.projectID,
					},
				},
			}
			adaptor := &Adaptor{}
			adaptor.Init(info)

			got, err := adaptor.GetRequestURL(info)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestServiceAccountExchangePreservesCancellationAndTLSVerification(t *testing.T) {
	var exchanges atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		_, _ = w.Write([]byte(`{"access_token":"must-not-be-accepted"}`))
	}))
	defer server.Close()
	previousTransport, previousInsecure := http.DefaultTransport, common.TLSInsecureSkipVerify
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
	http.DefaultTransport, common.TLSInsecureSkipVerify = transport, false
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("NO_PROXY", "*")
	service.InitHttpClient()
	t.Cleanup(func() {
		service.GetHttpClient().CloseIdleConnections()
		http.DefaultTransport, common.TLSInsecureSkipVerify = previousTransport, previousInsecure
		service.InitHttpClient()
	})
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	encoded, err := x509.MarshalPKCS8PrivateKey(privateKey)
	require.NoError(t, err)
	adaptor := &Adaptor{AccountCredentials: Credentials{
		ClientEmail: "service@example.test",
		PrivateKey:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})),
	}}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{
		ChannelId:      -20261023,
		ChannelSetting: dto.ChannelSettings{TLSInsecureSkipVerify: true},
	}}
	_, err = getAccessToken(context.Background(), adaptor, info)
	require.Error(t, err, "channel certificate bypass must never apply to Google's credential exchange")
	var certificateError *tls.CertificateVerificationError
	require.ErrorAs(t, err, &certificateError)
	assert.Zero(t, exchanges.Load(), "credentials must not be posted to an untrusted TLS server")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = exchangeJwtForAccessTokenWithProxy(ctx, "test-only-assertion", "")
	require.ErrorIs(t, err, context.Canceled)
	assert.NotContains(t, err.Error(), "test-only-assertion")
	assert.Zero(t, exchanges.Load())
}
