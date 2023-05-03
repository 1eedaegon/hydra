// Copyright © 2022 Ory Corp
// SPDX-License-Identifier: Apache-2.0

package oauth2_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	hydra "github.com/ory/hydra-client-go/v2"
	hc "github.com/ory/hydra/v2/client"
	"github.com/ory/hydra/v2/driver/config"
	"github.com/ory/hydra/v2/internal"
	"github.com/ory/hydra/v2/internal/testhelpers"
	"github.com/ory/hydra/v2/x"
	"github.com/ory/x/contextx"
	"github.com/ory/x/pointerx"
	"github.com/ory/x/requirex"
	"github.com/pborman/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"golang.org/x/oauth2"
)

func BenchmarkAuthCode(b *testing.B) {
	ctx := context.Background()

	spans := tracetest.NewSpanRecorder()
	tracer := trace.NewTracerProvider(trace.WithSpanProcessor(spans)).Tracer("")

	dsn := "postgres://postgres:secret@127.0.0.1:3445/postgres?sslmode=disable"
	reg := internal.NewRegistrySQLFromURL(b, dsn, true, new(contextx.Default)).WithTracer(tracer)
	reg.Config().MustSet(ctx, config.KeyLogLevel, "error")
	reg.Config().MustSet(ctx, config.KeyAccessTokenStrategy, "opaque")
	reg.Config().MustSet(ctx, config.KeyRefreshTokenHookURL, "")
	_, adminTS := testhelpers.NewOAuth2Server(ctx, b, reg)

	newOAuth2Client := func(b *testing.B, cb string) (*hc.Client, *oauth2.Config) {
		secret := uuid.New()
		c := &hc.Client{
			Secret:        secret,
			RedirectURIs:  []string{cb},
			ResponseTypes: []string{"id_token", "code", "token"},
			GrantTypes:    []string{"implicit", "refresh_token", "authorization_code", "password", "client_credentials"},
			Scope:         "hydra offline openid",
			Audience:      []string{"https://api.ory.sh/"},
		}
		require.NoError(b, reg.ClientManager().CreateClient(context.TODO(), c))
		return c, &oauth2.Config{
			ClientID:     c.GetID(),
			ClientSecret: secret,
			Endpoint: oauth2.Endpoint{
				AuthURL:   reg.Config().OAuth2AuthURL(ctx).String(),
				TokenURL:  reg.Config().OAuth2TokenURL(ctx).String(),
				AuthStyle: oauth2.AuthStyleInHeader,
			},
			Scopes: strings.Split(c.Scope, " "),
		}
	}

	adminClient := hydra.NewAPIClient(hydra.NewConfiguration())
	adminClient.GetConfig().Servers = hydra.ServerConfigurations{{URL: adminTS.URL}}

	getAuthorizeCode := func(b *testing.B, conf *oauth2.Config, c *http.Client, params ...oauth2.AuthCodeOption) (string, *http.Response) {
		if c == nil {
			c = testhelpers.NewEmptyJarClient(b)
		}

		state := uuid.New()
		resp, err := c.Get(conf.AuthCodeURL(state, params...))
		require.NoError(b, err)
		defer resp.Body.Close()

		q := resp.Request.URL.Query()
		require.EqualValues(b, state, q.Get("state"))
		return q.Get("code"), resp
	}

	acceptLoginHandler := func(b *testing.B, c *hc.Client, subject string, checkRequestPayload func(request *hydra.OAuth2LoginRequest) *hydra.AcceptOAuth2LoginRequest) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			rr, _, err := adminClient.OAuth2Api.GetOAuth2LoginRequest(context.Background()).LoginChallenge(r.URL.Query().Get("login_challenge")).Execute()
			require.NoError(b, err)

			assert.EqualValues(b, c.GetID(), pointerx.StringR(rr.Client.ClientId))
			assert.Empty(b, pointerx.StringR(rr.Client.ClientSecret))
			assert.EqualValues(b, c.GrantTypes, rr.Client.GrantTypes)
			assert.EqualValues(b, c.LogoURI, pointerx.StringR(rr.Client.LogoUri))
			assert.EqualValues(b, c.RedirectURIs, rr.Client.RedirectUris)
			assert.EqualValues(b, r.URL.Query().Get("login_challenge"), rr.Challenge)
			assert.EqualValues(b, []string{"hydra", "offline", "openid"}, rr.RequestedScope)
			assert.Contains(b, rr.RequestUrl, reg.Config().OAuth2AuthURL(ctx).String())

			acceptBody := hydra.AcceptOAuth2LoginRequest{
				Subject:  subject,
				Remember: pointerx.Bool(!rr.Skip),
				Acr:      pointerx.String("1"),
				Amr:      []string{"pwd"},
				Context:  map[string]interface{}{"context": "bar"},
			}
			if checkRequestPayload != nil {
				if b := checkRequestPayload(rr); b != nil {
					acceptBody = *b
				}
			}

			v, _, err := adminClient.OAuth2Api.AcceptOAuth2LoginRequest(context.Background()).
				LoginChallenge(r.URL.Query().Get("login_challenge")).
				AcceptOAuth2LoginRequest(acceptBody).
				Execute()
			require.NoError(b, err)
			require.NotEmpty(b, v.RedirectTo)
			http.Redirect(w, r, v.RedirectTo, http.StatusFound)
		}
	}

	acceptConsentHandler := func(b *testing.B, c *hc.Client, subject string, checkRequestPayload func(*hydra.OAuth2ConsentRequest)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			rr, _, err := adminClient.OAuth2Api.GetOAuth2ConsentRequest(context.Background()).ConsentChallenge(r.URL.Query().Get("consent_challenge")).Execute()
			require.NoError(b, err)

			assert.EqualValues(b, c.GetID(), pointerx.StringR(rr.Client.ClientId))
			assert.Empty(b, pointerx.StringR(rr.Client.ClientSecret))
			assert.EqualValues(b, c.GrantTypes, rr.Client.GrantTypes)
			assert.EqualValues(b, c.LogoURI, pointerx.StringR(rr.Client.LogoUri))
			assert.EqualValues(b, c.RedirectURIs, rr.Client.RedirectUris)
			assert.EqualValues(b, subject, pointerx.StringR(rr.Subject))
			assert.EqualValues(b, []string{"hydra", "offline", "openid"}, rr.RequestedScope)
			assert.EqualValues(b, r.URL.Query().Get("consent_challenge"), rr.Challenge)
			assert.Contains(b, *rr.RequestUrl, reg.Config().OAuth2AuthURL(ctx).String())
			if checkRequestPayload != nil {
				checkRequestPayload(rr)
			}

			assert.Equal(b, map[string]interface{}{"context": "bar"}, rr.Context)
			v, _, err := adminClient.OAuth2Api.AcceptOAuth2ConsentRequest(context.Background()).
				ConsentChallenge(r.URL.Query().Get("consent_challenge")).
				AcceptOAuth2ConsentRequest(hydra.AcceptOAuth2ConsentRequest{
					GrantScope: []string{"hydra", "offline", "openid"}, Remember: pointerx.Bool(true), RememberFor: pointerx.Int64(0),
					GrantAccessTokenAudience: rr.RequestedAccessTokenAudience,
					Session: &hydra.AcceptOAuth2ConsentRequestSession{
						AccessToken: map[string]interface{}{"foo": "bar"},
						IdToken:     map[string]interface{}{"bar": "baz"},
					},
				}).
				Execute()
			require.NoError(b, err)
			require.NotEmpty(b, v.RedirectTo)
			http.Redirect(w, r, v.RedirectTo, http.StatusFound)
		}
	}

	assertRefreshToken := func(b *testing.B, token *oauth2.Token, c *oauth2.Config, expectedExp time.Time) {
		actualExp, err := strconv.ParseInt(testhelpers.IntrospectToken(b, c, token.RefreshToken, adminTS).Get("exp").String(), 10, 64)
		require.NoError(b, err)
		requirex.EqualTime(b, expectedExp, time.Unix(actualExp, 0), time.Second)
	}

	assertIDToken := func(b *testing.B, token *oauth2.Token, c *oauth2.Config, expectedSubject, expectedNonce string, expectedExp time.Time) gjson.Result {
		idt, ok := token.Extra("id_token").(string)
		require.True(b, ok)
		assert.NotEmpty(b, idt)

		body, err := x.DecodeSegment(strings.Split(idt, ".")[1])
		require.NoError(b, err)

		claims := gjson.ParseBytes(body)
		assert.True(b, time.Now().After(time.Unix(claims.Get("iat").Int(), 0)), "%s", claims)
		assert.True(b, time.Now().After(time.Unix(claims.Get("nbf").Int(), 0)), "%s", claims)
		assert.True(b, time.Now().Before(time.Unix(claims.Get("exp").Int(), 0)), "%s", claims)
		requirex.EqualTime(b, expectedExp, time.Unix(claims.Get("exp").Int(), 0), 2*time.Second)
		assert.NotEmpty(b, claims.Get("jti").String(), "%s", claims)
		assert.EqualValues(b, reg.Config().IssuerURL(ctx).String(), claims.Get("iss").String(), "%s", claims)
		assert.NotEmpty(b, claims.Get("sid").String(), "%s", claims)
		assert.Equal(b, "1", claims.Get("acr").String(), "%s", claims)
		require.Len(b, claims.Get("amr").Array(), 1, "%s", claims)
		assert.EqualValues(b, "pwd", claims.Get("amr").Array()[0].String(), "%s", claims)

		require.Len(b, claims.Get("aud").Array(), 1, "%s", claims)
		assert.EqualValues(b, c.ClientID, claims.Get("aud").Array()[0].String(), "%s", claims)
		assert.EqualValues(b, expectedSubject, claims.Get("sub").String(), "%s", claims)
		assert.EqualValues(b, expectedNonce, claims.Get("nonce").String(), "%s", claims)
		assert.EqualValues(b, `baz`, claims.Get("bar").String(), "%s", claims)

		return claims
	}

	introspectAccessToken := func(b *testing.B, conf *oauth2.Config, token *oauth2.Token, expectedSubject string) gjson.Result {
		require.NotEmpty(b, token.AccessToken)
		i := testhelpers.IntrospectToken(b, conf, token.AccessToken, adminTS)
		assert.True(b, i.Get("active").Bool(), "%s", i)
		assert.EqualValues(b, conf.ClientID, i.Get("client_id").String(), "%s", i)
		assert.EqualValues(b, expectedSubject, i.Get("sub").String(), "%s", i)
		assert.EqualValues(b, `bar`, i.Get("ext.foo").String(), "%s", i)
		return i
	}

	assertJWTAccessToken := func(b *testing.B, strat string, conf *oauth2.Config, token *oauth2.Token, expectedSubject string, expectedExp time.Time) gjson.Result {
		require.NotEmpty(b, token.AccessToken)
		parts := strings.Split(token.AccessToken, ".")
		if strat != "jwt" {
			require.Len(b, parts, 2)
			return gjson.Parse("null")
		}
		require.Len(b, parts, 3)

		body, err := x.DecodeSegment(parts[1])
		require.NoError(b, err)

		i := gjson.ParseBytes(body)
		assert.NotEmpty(b, i.Get("jti").String())
		assert.EqualValues(b, conf.ClientID, i.Get("client_id").String(), "%s", i)
		assert.EqualValues(b, expectedSubject, i.Get("sub").String(), "%s", i)
		assert.EqualValues(b, reg.Config().IssuerURL(ctx).String(), i.Get("iss").String(), "%s", i)
		assert.True(b, time.Now().After(time.Unix(i.Get("iat").Int(), 0)), "%s", i)
		assert.True(b, time.Now().After(time.Unix(i.Get("nbf").Int(), 0)), "%s", i)
		assert.True(b, time.Now().Before(time.Unix(i.Get("exp").Int(), 0)), "%s", i)
		requirex.EqualTime(b, expectedExp, time.Unix(i.Get("exp").Int(), 0), time.Second)
		assert.EqualValues(b, `bar`, i.Get("ext.foo").String(), "%s", i)
		assert.EqualValues(b, `["hydra","offline","openid"]`, i.Get("scp").Raw, "%s", i)
		return i
	}

	subject := "aeneas-rekkas"
	nonce := uuid.New()

	run := func(b *testing.B, strategy string) {
		c, conf := newOAuth2Client(b, testhelpers.NewCallbackURL(b, "callback", testhelpers.HTTPServerNotImplementedHandler))
		testhelpers.NewLoginConsentUI(b, reg.Config(),
			acceptLoginHandler(b, c, subject, nil),
			acceptConsentHandler(b, c, subject, nil),
		)

		code, _ := getAuthorizeCode(b, conf, nil, oauth2.SetAuthURLParam("nonce", nonce))
		require.NotEmpty(b, code)
		token, err := conf.Exchange(context.Background(), code)
		iat := time.Now()
		require.NoError(b, err)

		introspectAccessToken(b, conf, token, subject)
		assertJWTAccessToken(b, strategy, conf, token, subject, iat.Add(reg.Config().GetAccessTokenLifespan(ctx)))
		assertIDToken(b, token, conf, subject, nonce, iat.Add(reg.Config().GetIDTokenLifespan(ctx)))
		assertRefreshToken(b, token, conf, iat.Add(reg.Config().GetRefreshTokenLifespan(ctx)))
	}

	b.Run("strategy=jwt", func(b *testing.B) {
		reg.Config().MustSet(ctx, config.KeyAccessTokenStrategy, "jwt")
		initialDBSpans := dbSpans(spans)
		for i := 0; i < b.N; i++ {
			run(b, "jwt")
		}
		b.ReportMetric(0, "ns/op")
		b.ReportMetric(float64(b.Elapsed().Milliseconds())/float64(b.N), "ms/op")
		b.ReportMetric((float64(dbSpans(spans)-initialDBSpans))/float64(b.N), "queries/op")
	})

	b.Run("strategy=opaque", func(b *testing.B) {
		reg.Config().MustSet(ctx, config.KeyAccessTokenStrategy, "opaque")
		initialDBSpans := dbSpans(spans)
		for i := 0; i < b.N; i++ {
			run(b, "opaque")
		}
		b.ReportMetric(0, "ns/op")
		b.ReportMetric(float64(b.Elapsed().Milliseconds())/float64(b.N), "ms/op")
		b.ReportMetric((float64(dbSpans(spans)-initialDBSpans))/float64(b.N), "queries/op")
	})

}
