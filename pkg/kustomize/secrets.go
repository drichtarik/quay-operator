package kustomize

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/go-logr/logr"
	"github.com/quay/clair/v4/config"
	"github.com/quay/clair/v4/notifier/webhook"
	"github.com/quay/config-tool/pkg/lib/fieldgroups/database"
	"github.com/quay/config-tool/pkg/lib/fieldgroups/distributedstorage"
	"github.com/quay/config-tool/pkg/lib/fieldgroups/hostsettings"
	"github.com/quay/config-tool/pkg/lib/fieldgroups/redis"
	"github.com/quay/config-tool/pkg/lib/fieldgroups/securityscanner"
	"github.com/quay/config-tool/pkg/lib/shared"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/cert"
	"sigs.k8s.io/yaml"

	v1 "github.com/quay/quay-operator/api/v1"
)

const (
	// secretKeySecretName is the name of the Secret in which generated secret keys are stored.
	secretKeySecretName = "quay-registry-managed-secret-keys"
	secretKeyLength     = 80
)

// SecretKeySecretName returns the name of the Secret in which generated secret keys are stored.
func SecretKeySecretName(quay *v1.QuayRegistry) string {
	return quay.GetName() + "-" + secretKeySecretName
}

// generateKeyIfMissing checks if the given key is in the parsed config. If not, the secretKeysSecret
// is checked for the key. If not present, a new key is generated.
func generateKeyIfMissing(parsedConfig map[string]interface{}, secretKeysSecret *corev1.Secret, keyName string, quay *v1.QuayRegistry, log logr.Logger) (string, *corev1.Secret) {
	// Check for the user-given key in config.
	found, ok := parsedConfig[keyName]
	if ok {
		log.Info("Secret key found in provided config", "keyName", keyName)
		return found.(string), secretKeysSecret
	}

	// If not found in the given config, check the secret keys Secret.
	if secretKeysSecret == nil {
		log.Info("Creating a new secret key Secret")
		secretKeysSecret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      SecretKeySecretName(quay),
				Namespace: quay.Namespace,
			},
			StringData: map[string]string{},
		}
	}

	foundSecretKey, ok := secretKeysSecret.Data[keyName]
	if ok {
		log.Info("Secret key found in managed secret", "keyName", keyName)
		return string(foundSecretKey), secretKeysSecret
	} else {
		log.Info("Generating secret key", "keyName", keyName)
		generatedSecretKey, err := generateRandomString(secretKeyLength)
		check(err)

		stringData := secretKeysSecret.StringData
		if stringData == nil {
			stringData = map[string]string{}
		}

		secretKeysSecret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      SecretKeySecretName(quay),
				Namespace: quay.Namespace,
			},
			Data:       secretKeysSecret.Data,
			StringData: stringData,
		}

		secretKeysSecret.StringData[keyName] = generatedSecretKey
		return generatedSecretKey, secretKeysSecret
	}
}

// handleSecretKeys generates any secret keys not already present in the config bundle and adds them
// to the specialized secretKeysSecret.
func handleSecretKeys(parsedConfig map[string]interface{}, secretKeysSecret *corev1.Secret, quay *v1.QuayRegistry, log logr.Logger) (string, string, *corev1.Secret) {
	// Check for SECRET_KEY and DATABASE_SECRET_KEY. If not present, generate them
	// and place them into their own Secret.
	secretKey, secretKeysSecret := generateKeyIfMissing(parsedConfig, secretKeysSecret, "SECRET_KEY", quay, log)
	databaseSecretKey, secretKeysSecret := generateKeyIfMissing(parsedConfig, secretKeysSecret, "DATABASE_SECRET_KEY", quay, log)
	return secretKey, databaseSecretKey, secretKeysSecret
}

// FieldGroupFor generates and returns the correct config field group for the given component.
func FieldGroupFor(component string, quay *v1.QuayRegistry) (shared.FieldGroup, error) {
	switch component {
	case "clair":
		fieldGroup, err := securityscanner.NewSecurityScannerFieldGroup(map[string]interface{}{})
		if err != nil {
			return nil, err
		}

		fieldGroup.FeatureSecurityScanner = true
		fieldGroup.SecurityScannerV4Endpoint = "http://" + quay.GetName() + "-" + "clair:80"
		fieldGroup.SecurityScannerV4NamespaceWhitelist = []string{"admin"}

		return fieldGroup, nil
	case "redis":
		fieldGroup, err := redis.NewRedisFieldGroup(map[string]interface{}{})
		if err != nil {
			return nil, err
		}

		fieldGroup.BuildlogsRedis = &redis.BuildlogsRedisStruct{
			Host: strings.Join([]string{quay.GetName(), "quay-redis"}, "-"),
			Port: 6379,
		}
		fieldGroup.UserEventsRedis = &redis.UserEventsRedisStruct{
			Host: strings.Join([]string{quay.GetName(), "quay-redis"}, "-"),
			Port: 6379,
		}

		return fieldGroup, nil
	case "postgres":
		fieldGroup, err := database.NewDatabaseFieldGroup(map[string]interface{}{})
		if err != nil {
			return nil, err
		}
		user := "postgres"
		// FIXME(alecmerdler): Make this more secure...
		password := "postgres"
		host := strings.Join([]string{quay.GetName(), "quay-postgres"}, "-")
		port := "5432"
		name := "quay"
		fieldGroup.DbUri = fmt.Sprintf("postgresql://%s:%s@%s:%s/%s", user, password, host, port, name)

		return fieldGroup, nil
	case "objectstorage":
		hostname := quay.GetAnnotations()[v1.StorageHostnameAnnotation]
		bucketName := quay.GetAnnotations()[v1.StorageBucketNameAnnotation]
		accessKey := quay.GetAnnotations()[v1.StorageAccessKeyAnnotation]
		secretKey := quay.GetAnnotations()[v1.StorageSecretKeyAnnotation]

		fieldGroup := &distributedstorage.DistributedStorageFieldGroup{
			FeatureProxyStorage:                true,
			DistributedStoragePreference:       []string{"local_us"},
			DistributedStorageDefaultLocations: []string{"local_us"},
			DistributedStorageConfig: map[string]*distributedstorage.DistributedStorageDefinition{
				"local_us": {
					Name: "RadosGWStorage",
					Args: &shared.DistributedStorageArgs{
						Hostname:    hostname,
						IsSecure:    true,
						Port:        443,
						StoragePath: "/datastorage/registry",
						BucketName:  bucketName,
						AccessKey:   accessKey,
						SecretKey:   secretKey,
					},
				},
			},
		}

		return fieldGroup, nil
	case "route":
		clusterHostname := quay.GetAnnotations()[v1.ClusterHostnameAnnotation]

		fieldGroup := &hostsettings.HostSettingsFieldGroup{
			ExternalTlsTermination: false,
			PreferredUrlScheme:     "https",
			ServerHostname: strings.Join([]string{
				strings.Join([]string{quay.GetName(), "quay", quay.GetNamespace()}, "-"),
				clusterHostname},
				"."),
		}

		return fieldGroup, nil
	case "horizontalpodautoscaler":
		return nil, nil
	default:
		return nil, errors.New("unknown component: " + component)
	}
}

// BaseConfig returns a minimum config bundle with values that Quay doesn't have defaults for.
func BaseConfig() map[string]interface{} {
	return map[string]interface{}{
		"FEATURE_MAILING":                    false,
		"REGISTRY_TITLE":                     "Quay",
		"REGISTRY_TITLE_SHORT":               "Quay",
		"AUTHENTICATION_TYPE":                "Database",
		"ENTERPRISE_LOGO_URL":                "/static/img/quay-horizontal-color.svg",
		"DEFAULT_TAG_EXPIRATION":             "2w",
		"ALLOW_PULLS_WITHOUT_STRICT_LOGGING": false,
		"TAG_EXPIRATION_OPTIONS":             []string{"2w"},
		"TEAM_RESYNC_STALE_TIME":             "60m",
		"FEATURE_DIRECT_LOGIN":               true,
		"FEATURE_BUILD_SUPPORT":              false,
	}
}

// CustomTLSFor generates a TLS certificate/key pair for the Quay registry to use for secure communication with clients.
func CustomTLSFor(quay *v1.QuayRegistry, baseConfig map[string]interface{}) ([]byte, []byte, error) {
	routeConfigFiles := configFilesFor("route", quay, baseConfig)
	var fieldGroup hostsettings.HostSettingsFieldGroup
	if err := yaml.Unmarshal(routeConfigFiles["route.config.yaml"], &fieldGroup); err != nil {
		return nil, nil, err
	}

	return cert.GenerateSelfSignedCertKey(fieldGroup.ServerHostname, []net.IP{}, []string{})
}

func configFilesFor(component string, quay *v1.QuayRegistry, baseConfig map[string]interface{}) map[string][]byte {
	configFiles := map[string][]byte{}
	fieldGroup, err := FieldGroupFor(component, quay)
	check(err)

	switch component {
	case "clair":
	case "postgres":
	case "redis":
	case "objectstorage":
	case "horizontalpodautoscaler":
	case "route":
		hostSettings := fieldGroup.(*hostsettings.HostSettingsFieldGroup)

		if hostname, ok := baseConfig["SERVER_HOSTNAME"]; ok {
			configFiles[registryHostnameKey] = []byte(hostname.(string))
			hostSettings.ServerHostname = hostname.(string)
		}
	default:
		panic("unknown component: " + component)
	}

	configFiles[component+".config.yaml"] = encode(fieldGroup)

	return configFiles
}

func fieldGroupFor(component string) string {
	switch component {
	case "clair":
		return "SecurityScanner"
	case "postgres":
		return "Database"
	case "redis":
		return "Redis"
	case "objectstorage":
		return "DistributedStorage"
	case "route":
		return "HostSettings"
	case "horizontalpodautoscaler":
		return ""
	default:
		panic("unknown component: " + component)
	}
}

// componentConfigFilesFor returns specific config files for managed components of a Quay registry.
func componentConfigFilesFor(component string, quay *v1.QuayRegistry) (map[string][]byte, error) {
	switch component {
	case "clair":
		return map[string][]byte{"config.yaml": clairConfigFor(quay)}, nil
	default:
		return nil, nil
	}
}

// clairConfigFor returns a Clair v4 config with the correct values.
func clairConfigFor(quay *v1.QuayRegistry) []byte {
	host := strings.Join([]string{quay.GetName(), "clair-postgres"}, "-")
	dbname := "clair"
	user := "postgres"
	// FIXME(alecmerdler): Make this more secure...
	password := "postgres"

	dbConn := fmt.Sprintf("host=%s port=5432 dbname=%s user=%s password=%s sslmode=disable", host, dbname, user, password)
	config := config.Config{
		HTTPListenAddr: ":8080",
		LogLevel:       "debug",
		Indexer: config.Indexer{
			ConnString:           dbConn,
			ScanLockRetry:        10,
			LayerScanConcurrency: 5,
			Migrations:           true,
		},
		Matcher: config.Matcher{
			ConnString:  dbConn,
			MaxConnPool: 100,
			Migrations:  true,
		},
		Notifier: config.Notifier{
			ConnString:       dbConn,
			Migrations:       true,
			DeliveryInterval: "1m",
			PollInterval:     "5m",
			Webhook: &webhook.Config{
				// FIXME(alecmerdler): Need to use HTTPS when Quay has a custom hostname + SSL cert/keys...
				Target:   "http://" + quay.GetName() + "-quay-app/secscan/notification",
				Callback: "http://" + quay.GetName() + "-clair/notifier/api/v1/notifications",
			},
		},
		// FIXME(alecmerdler): Create pre-shared key for JWT auth between Quay/Clair...
		// Auth: config.Auth{},
		Metrics: config.Metrics{
			Name: "prometheus",
		},
	}

	marshalled, err := yaml.Marshal(config)
	check(err)

	return marshalled
}

// From: https://gist.github.com/dopey/c69559607800d2f2f90b1b1ed4e550fb
// generateRandomBytes returns securely generated random bytes.
func generateRandomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := rand.Read(b)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// generateRandomString returns a securely generated random string.
func generateRandomString(n int) (string, error) {
	const letters = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-"
	bytes, err := generateRandomBytes(n)
	if err != nil {
		return "", err
	}
	for i, b := range bytes {
		bytes[i] = letters[b%byte(len(letters))]
	}
	return string(bytes), nil
}
