package nomad

import (
	"fmt"
	"log"
	"os"
	"reflect"
	"testing"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
	"github.com/hashicorp/vault/logical"
	"github.com/mitchellh/mapstructure"
	dockertest "gopkg.in/ory-am/dockertest.v3"
)

func prepareTestContainer(t *testing.T) (cleanup func(), retAddress string, nomadToken string) {
	nomadToken = os.Getenv("NOMAD_TOKEN")

	retAddress = os.Getenv("NOMAD_ADDR")

	if retAddress != "" {
		return func() {}, retAddress, nomadToken
	}

	pool, err := dockertest.NewPool("")
	if err != nil {
		t.Fatalf("Failed to connect to docker: %s", err)
	}

	dockerOptions := &dockertest.RunOptions{
		Repository: "djenriquez/nomad",
		Tag:        "v0.7.0-beta1",
		Cmd:        []string{"agent", "-dev"},
	}
	resource, err := pool.RunWithOptions(dockerOptions)
	if err != nil {
		t.Fatalf("Could not start local Nomad docker container: %s", err)
	}

	cleanup = func() {
		err := pool.Purge(resource)
		if err != nil {
			t.Fatalf("Failed to cleanup local container: %s", err)
		}
	}

	retAddress = fmt.Sprintf("http://localhost:%s/", resource.GetPort("4646/tcp"))

	// exponential backoff-retry
	if err = pool.Retry(func() error {
		var err error
		nomadapiConfig := nomadapi.DefaultConfig()
		nomadapiConfig.Address = retAddress
		nomad, err := nomadapi.NewClient(nomadapiConfig)
		if err != nil {
			return err
		}
		aclbootstrap, _, err := nomad.ACLTokens().Bootstrap(nil)
		nomadToken = aclbootstrap.SecretID
		policy := &nomadapi.ACLPolicy{
			Name:        "test",
			Description: "test",
			Rules: `namespace "default" {
        policy = "read"
      }
      `,
		}
		_, err = nomad.ACLPolicies().Upsert(policy, nil)
		if err != nil {
			t.Fatal(err)
		}
		return err
	}); err != nil {
		cleanup()
		t.Fatalf("Could not connect to docker: %s", err)
	}
	return cleanup, retAddress, nomadToken
}

func TestBackend_config_access(t *testing.T) {
	config := logical.TestBackendConfig()
	config.StorageView = &logical.InmemStorage{}
	b, err := Factory(config)
	if err != nil {
		t.Fatal(err)
	}

	cleanup, connURL, connToken := prepareTestContainer(t)
	defer cleanup()

	connData := map[string]interface{}{
		"address": connURL,
		"token":   connToken,
	}

	confReq := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "config/access",
		Storage:   config.StorageView,
		Data:      connData,
	}

	resp, err := b.HandleRequest(confReq)
	if err != nil || (resp != nil && resp.IsError()) || resp != nil {
		t.Fatalf("failed to write configuration: resp:%#v err:%s", resp, err)
	}

	confReq.Operation = logical.ReadOperation
	resp, err = b.HandleRequest(confReq)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("failed to write configuration: resp:%#v err:%s", resp, err)
	}

	expected := map[string]interface{}{
		"address": connData["address"].(string),
		"scheme":  "http",
	}
	if !reflect.DeepEqual(expected, resp.Data) {
		t.Fatalf("bad: expected:%#v\nactual:%#v\n", expected, resp.Data)
	}
	if resp.Data["token"] != nil {
		t.Fatalf("token should not be set in the response")
	}
}

func TestBackend_renew_revoke(t *testing.T) {
	config := logical.TestBackendConfig()
	config.StorageView = &logical.InmemStorage{}
	b, err := Factory(config)
	if err != nil {
		t.Fatal(err)
	}

	cleanup, connURL, connToken := prepareTestContainer(t)
	defer cleanup()

	connData := map[string]interface{}{
		"address": connURL,
		"token":   connToken,
	}

	req := &logical.Request{
		Storage:   config.StorageView,
		Operation: logical.UpdateOperation,
		Path:      "config/access",
		Data:      connData,
	}
	resp, err := b.HandleRequest(req)
	if err != nil {
		t.Fatal(err)
	}

	req.Path = "roles/test"
	req.Data = map[string]interface{}{
		"policy": []string{"policy"},
		"lease":  "6h",
	}
	resp, err = b.HandleRequest(req)
	if err != nil {
		t.Fatal(err)
	}

	req.Operation = logical.ReadOperation
	req.Path = "creds/test"
	resp, err = b.HandleRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil {
		t.Fatal("resp nil")
	}
	if resp.IsError() {
		t.Fatalf("resp is error: %v", resp.Error())
	}

	generatedSecret := resp.Secret
	generatedSecret.IssueTime = time.Now()
	generatedSecret.TTL = 6 * time.Hour

	var d struct {
		Token string `mapstructure:"SecretID"`
	}
	if err := mapstructure.Decode(resp.Data, &d); err != nil {
		t.Fatal(err)
	}
	log.Printf("[WARN] Generated token: %s", d.Token)

	// Build a client and verify that the credentials work
	nomadapiConfig := nomadapi.DefaultConfig()
	nomadapiConfig.Address = connData["address"].(string)
	nomadapiConfig.SecretID = d.Token
	client, err := nomadapi.NewClient(nomadapiConfig)
	if err != nil {
		t.Fatal(err)
	}

	log.Printf("[WARN] Verifying that the generated token works...")
	_, err = client.Status().Leader, nil
	if err != nil {
		t.Fatal(err)
	}

	req.Operation = logical.RenewOperation
	req.Secret = generatedSecret
	resp, err = b.HandleRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp == nil {
		t.Fatal("got nil response from renew")
	}

	req.Operation = logical.RevokeOperation
	resp, err = b.HandleRequest(req)
	if err != nil {
		t.Fatal(err)
	}

	log.Printf("[WARN] Verifying that the generated token does not work...")
	_, err = client.Status().Leader, nil
	if err == nil {
		t.Fatal("expected error")
	}
}