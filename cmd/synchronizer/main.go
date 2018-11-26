// synchronizer synchronizes Vault secrets with Kubernetes secrets
//
// synchronizer expects a valid Vault token in VAULT_TOKEN_PATH (see authenticator)
// all Kubernetes secrets receive an annotation to identify and delete them as synchronized secrets when they are no longer needed
//
// synchronizer is meant to be used in an init container on Kubernetes.
package main

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path"
	"strings"

	"github.com/hashicorp/vault/api"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	vaultAnnotation = "vault-secret"
)

func main() {
	c, err := newFromEnvironment()
	if err != nil {
		log.Fatal(errors.Wrap(err, "failed to get config"))
	}

	token, err := c.loadToken()
	if err != nil {
		if err := c.checkSecrets(); err != nil {
			log.Fatal(err)
		}
		// you get only here if ALLOW_FAIL=true was set for vault-kubernetes-auth Init Container and vault-kubernetes-auth failed to authenticate
		log.Println(errors.Wrap(err, "cannot synchronize secrets - all secrets seems to be available therefore pod creation will continue"))
		os.Exit(0)
	}

	if err := c.synchronize(token); err != nil {
		log.Fatal(errors.Wrap(err, "failed to synchronize secrets"))
	}
	log.Printf("secrets successfully synchronized")

	os.Exit(0)
}

type config struct {
	VaultTokenPath string
	Secrets        map[string]string // key = kubernetes secret name, value = vault secret name
	Namespace      string
	k8sClientset   *kubernetes.Clientset
	vaultClient    *api.Client
}

func newFromEnvironment() (*config, error) {
	c := &config{}
	c.VaultTokenPath = os.Getenv("VAULT_TOKEN_PATH")
	if c.VaultTokenPath == "" {
		return nil, fmt.Errorf("missing VAULT_TOKEN_PATH")
	}
	c.Secrets = make(map[string]string)
	for _, item := range strings.Split(os.Getenv("VAULT_SECRETS"), ",") {
		if len(item) == 0 {
			continue
		}
		s := strings.Split(item, ":")
		k := path.Base(s[0])
		if len(s) > 1 {
			k = s[1]
		}
		c.Secrets[k] = s[0]
	}
	if len(c.Secrets) == 0 {
		return nil, fmt.Errorf("no secrets to synchronize - check VAULT_SECRETS")
	}
	// current kubernetes namespace
	content, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return nil, errors.Wrap(err, "could not get namespace")
	}
	c.Namespace = strings.TrimSpace(string(content))
	// connect to kubernetes
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get k8s config")
	}
	c.k8sClientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get k8s k8sClientset")
	}
	// connect to vault
	vaultConfig := api.DefaultConfig()
	if err := vaultConfig.ReadEnvironment(); err != nil {
		return nil, errors.Wrap(err, "failed to read environment for vault")
	}
	c.vaultClient, err = api.NewClient(vaultConfig)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create vault client")
	}
	return c, nil
}

// loadToken from VaultTokenPath
func (c *config) loadToken() (string, error) {
	content, err := ioutil.ReadFile(c.VaultTokenPath)
	if err != nil {
		return "", errors.Wrap(err, "could not get vault token")
	}
	return string(content), nil
}

// checkSecrets check the existence of a secret and not the content
func (c *config) checkSecrets() error {
	// check secrets
	for k, v := range c.Secrets {
		log.Println("check secret", k, "from vault secret", v)
		_, err := c.k8sClientset.CoreV1().Secrets(c.Namespace).Get(k, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("secret %s does not exist", k)
		}
	}
	return nil
}

// synchronize secret from vault to the current kubernetes namespace
func (c *config) synchronize(token string) error {
	c.vaultClient.SetToken(token)
	// create/update the secrets
	annotations := make(map[string]string)
	for k, v := range c.Secrets {
		// get secret from vault
		log.Println("read", v, "from vault")
		s, err := c.vaultClient.Logical().Read(v)
		if err != nil {
			return err
		}
		// convert data
		data := make(map[string][]byte)
		for k, v := range s.Data["data"].(map[string]interface{}) {
			data[k] = []byte(v.(string))
		}
		// create/update k8s secret
		annotations[vaultAnnotation] = v
		secret := &corev1.Secret{}
		secret.Name = k
		secret.Data = data
		secret.Annotations = annotations
		// create (insert) or update the secret
		_, err = c.k8sClientset.CoreV1().Secrets(c.Namespace).Get(secret.Name, metav1.GetOptions{})
		if apierr.IsNotFound(err) {
			log.Println("create secret", secret.Name, "from vault secret", v)
			if _, err := c.k8sClientset.CoreV1().Secrets(c.Namespace).Create(secret); err != nil {
				return err
			}
			continue
		}
		log.Println("update secret", secret.Name, "from vault secret", v)
		if _, err = c.k8sClientset.CoreV1().Secrets(c.Namespace).Update(secret); err != nil {
			return err
		}
	}
	// delete obsolete secrets
	secretList, err := c.k8sClientset.CoreV1().Secrets(c.Namespace).List(metav1.ListOptions{})
	if err != nil {
		log.Println(errors.Wrap(err, "cleanup of unused vault secrets failed"))
		os.Exit(0)
	}
	for _, s := range secretList.Items {
		// only secrets from vault
		if _, ok := s.Annotations[vaultAnnotation]; !ok {
			continue
		}
		// only if vault secret is not in secrets
		if _, ok := c.Secrets[s.Name]; ok {
			continue
		}
		log.Println("delete secret", s.Name)
		if err := c.k8sClientset.CoreV1().Secrets(c.Namespace).Delete(s.Name, &metav1.DeleteOptions{}); err != nil {
			log.Println(errors.Wrapf(err, "delete obsolete vault secret %s failed", s.Name))
		}
	}
	return nil
}
