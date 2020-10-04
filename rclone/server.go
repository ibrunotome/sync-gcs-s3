// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"fmt"
	"log"
	"os"

	_ "github.com/rclone/rclone/backend/all"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/sync"

	"errors"
	"net/http"
	"strings"

	jwt "github.com/dgrijalva/jwt-go"
	"github.com/lestrrat/go-jwx/jwk"
	"golang.org/x/net/http2"

	"github.com/gorilla/mux"
)

type gcpIdentityDoc struct {
	Email           string `json:"email,omitempty"`
	EmailVerified   bool   `json:"email_verified,omitempty"`
	AuthorizedParty string `json:"azp,omitempty"`
	jwt.StandardClaims
}

type contextKey string

const (
	jwksURL                    = "https://www.googleapis.com/oauth2/v3/certs"
	contextEventKey contextKey = "event"
)

var (
	jwtSet            *jwk.Set
	gs                = os.Getenv("GS")
	s3                = os.Getenv("S3")
	awsAccessKey      = os.Getenv("AWS_ACCESS_KEY_ID")
	awsSecretAcessKey = os.Getenv("AWS_SECRET_ACCESS_ID")
	awsRegion         = os.Getenv("AWS_REGION")
	myAudience        = os.Getenv("AUDIENCE")
)

func getKey(token *jwt.Token) (interface{}, error) {
	keyID, ok := token.Header["kid"].(string)
	if !ok {
		return nil, errors.New("expecting JWT header to have string kid")
	}
	if key := jwtSet.LookupKeyID(keyID); len(key) == 1 {
		return key[0].Materialize()
	}
	return nil, errors.New("unable to find key")
}

func verifyGoogleIDToken(ctx context.Context, aud string, rawToken string) (gcpIdentityDoc, error) {
	token, err := jwt.ParseWithClaims(rawToken, &gcpIdentityDoc{}, getKey)
	if err != nil {
		log.Printf("Error parsing JWT %v", err)
		return gcpIdentityDoc{}, err
	}
	if claims, ok := token.Claims.(*gcpIdentityDoc); ok && token.Valid {
		log.Printf("OIDC doc has Audience [%s]   Issuer [%v]", claims.Audience, claims.StandardClaims.Issuer)
		return *claims, nil
	}
	return gcpIdentityDoc{}, errors.New("Error parsing JWT Claims")
}

func authMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println("/authMiddleware called")

		authHeader := r.Header.Get("Authorization")

		if authHeader == "" {
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}
		splitToken := strings.Split(authHeader, "Bearer")
		if len(splitToken) > 0 {
			tok := strings.TrimSpace(splitToken[1])
			idDoc, err := verifyGoogleIDToken(r.Context(), myAudience, tok)
			if err != nil {
				http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
				return
			}
			// TODO: optionally validate the inbound service account for Cloud Scheduler here.
			log.Printf("Authenticated email: %v", idDoc.Email)
			// Emit the id token into the request Context
			ctx := context.WithValue(r.Context(), contextEventKey, idDoc)
			h.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
		return
	})
}

func defaulthandler(w http.ResponseWriter, r *http.Request) {
	fsrc, err := fs.NewFs(fmt.Sprintf("gs:%s", gs))
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	fdest, err := fs.NewFs(fmt.Sprintf("s3:%s", s3))
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	err = sync.Sync(r.Context(), fdest, fsrc, false)
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}

	fmt.Fprint(w, "ok")
}

func main() {

	var err error
	jwtSet, err = jwk.FetchHTTP(jwksURL)
	if err != nil {
		log.Fatal("Unable to load JWK Set: ", err)
	}

	if myAudience == "" || gs == "" || s3 == "" {
		log.Fatalln("Audience, gs, s3 values must be set")
	}

	// Configure the source and destination
	fs.ConfigFileSet("gs", "type", "google cloud storage")
	fs.ConfigFileSet("gs", "bucket_policy_only", "true")

	fs.ConfigFileSet("s3", "type", "s3")
	fs.ConfigFileSet("s3", "provider", "AWS")
	fs.ConfigFileSet("s3", "env_auth", "false")
	fs.ConfigFileSet("s3", "access_key_id", awsAccessKey)
	fs.ConfigFileSet("s3", "secret_access_key", awsSecretAcessKey)
	fs.ConfigFileSet("s3", "region", awsRegion)
	fs.ConfigFileSet("s3", "acl", "private")
	fs.ConfigFileSet("s3", "bucket_acl", "private")

	router := mux.NewRouter()
	router.Methods(http.MethodGet).Path("/").HandlerFunc(defaulthandler)

	var server *http.Server
	server = &http.Server{
		Addr:    ":8080",
		Handler: authMiddleware(router),
	}
	http2.ConfigureServer(server, &http2.Server{})

	err = server.ListenAndServe()
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
