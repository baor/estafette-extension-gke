package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/alecthomas/kingpin"
)

var (
	app       string
	version   string
	branch    string
	revision  string
	buildDate string
	goVersion = runtime.Version()
)

var (
	// flags
	paramsJSON      = kingpin.Flag("params", "Extension parameters, created from custom properties.").Envar("ESTAFETTE_EXTENSION_CUSTOM_PROPERTIES").Required().String()
	credentialsJSON = kingpin.Flag("credentials", "GKE credentials configured at service level, passed in to this trusted extension.").Envar("ESTAFETTE_CREDENTIALS_KUBERNETES_ENGINE").Required().String()

	// optional flags
	appLabel      = kingpin.Flag("app-name", "App label, used as application name if not passed explicitly.").Envar("ESTAFETTE_LABEL_APP").String()
	buildVersion  = kingpin.Flag("build-version", "Version number, used if not passed explicitly.").Envar("ESTAFETTE_BUILD_VERSION").String()
	releaseName   = kingpin.Flag("release-name", "Name of the release section, which is used by convention to resolve the credentials.").Envar("ESTAFETTE_RELEASE_NAME").String()
	releaseAction = kingpin.Flag("release-action", "Name of the release action, to control the type of release.").Envar("ESTAFETTE_RELEASE_ACTION").String()

	assistTroubleshootingOnError = false
	paramsForTroubleshooting     = Params{}
)

func main() {

	// parse command line parameters
	kingpin.Parse()

	// log to stdout and hide timestamp
	log.SetOutput(os.Stdout)
	log.SetFlags(log.Flags() &^ (log.Ldate | log.Ltime))

	// log startup message
	logInfo("Starting %v version %v...", app, version)

	// put all estafette labels in map
	logInfo("Getting all estafette labels from envvars...")
	estafetteLabels := map[string]string{}
	for _, e := range os.Environ() {
		kvPair := strings.SplitN(e, "=", 2)

		if len(kvPair) == 2 {
			envvarName := kvPair[0]
			envvarValue := kvPair[1]

			if strings.HasPrefix(envvarName, "ESTAFETTE_LABEL_") {
				// strip prefix and convert to lowercase
				key := strings.ToLower(strings.Replace(envvarName, "ESTAFETTE_LABEL_", "", 1))
				estafetteLabels[key] = envvarValue
			}
		}
	}

	logInfo("Unmarshalling credentials parameter...\n")
	var credentialsParam CredentialsParam
	err := json.Unmarshal([]byte(*paramsJSON), &credentialsParam)
	if err != nil {
		log.Fatal("Failed unmarshalling credential parameter: ", err)
	}

	logInfo("Setting default for credential parameter...")
	credentialsParam.SetDefaults(*releaseName)

	logInfo("Validating required credential parameter...")
	valid, errors := credentialsParam.ValidateRequiredProperties()
	if !valid {
		log.Fatal("Not all valid fields are set: ", errors)
	}

	logInfo("Unmarshalling injected credentials...")
	var credentials []GKECredentials
	err = json.Unmarshal([]byte(*credentialsJSON), &credentials)
	if err != nil {
		log.Fatal("Failed unmarshalling injected credentials: ", err)
	}

	logInfo("Checking if credential %v exists...", credentialsParam.Credentials)
	credential := GetCredentialsByName(credentials, credentialsParam.Credentials)
	if credential == nil {
		log.Fatalf("Credential with name %v does not exist.", credentialsParam.Credentials)
	}

	var params Params
	if credential.AdditionalProperties.Defaults != nil {
		logInfo("Using defaults from credential %v...", credentialsParam.Credentials)
		// todo log just the specified defaults, not the entire parms object
		// defaultsAsYAML, err := yaml.Marshal(credential.AdditionalProperties.Defaults)
		// if err == nil {
		// 	log.Printf(string(defaultsAsYAML))
		// }
		params = *credential.AdditionalProperties.Defaults
	}

	logInfo("Unmarshalling parameters / custom properties...")
	err = json.Unmarshal([]byte(*paramsJSON), &params)
	if err != nil {
		log.Fatal("Failed unmarshalling parameters: ", err)
	}

	logInfo("Setting defaults for parameters that are not set in the manifest...")
	params.SetDefaults(*appLabel, *buildVersion, *releaseName, *releaseAction, estafetteLabels)

	logInfo("Validating required parameters...")
	valid, errors = params.ValidateRequiredProperties()
	if !valid {
		log.Fatal("Not all valid fields are set: ", errors)
	}

	// combine templates
	tmpl, err := buildTemplates(params)
	if err != nil {
		log.Fatal("Failed building templates: ", err)
	}

	// pre-render config files if they exist
	params.Configs.RenderedFileContent = renderConfig(params)

	// generate the data required for rendering the templates
	templateData := generateTemplateData(params)

	// render the template
	renderedTemplate, err := renderTemplate(tmpl, templateData)
	if err != nil {
		log.Fatal("Failed rendering templates: ", err)
	}

	if tmpl != nil {
		logInfo("Storing rendered manifest on disk...")
		err = ioutil.WriteFile("/kubernetes.yaml", renderedTemplate.Bytes(), 0600)
		if err != nil {
			log.Fatal("Failed writing manifest: ", err)
		}
	}

	logInfo("Retrieving service account email from credentials...")
	var keyFileMap map[string]interface{}
	err = json.Unmarshal([]byte(credential.AdditionalProperties.ServiceAccountKeyfile), &keyFileMap)
	if err != nil {
		log.Fatal("Failed unmarshalling service account keyfile: ", err)
	}
	var saClientEmail string
	if saClientEmailIntfc, ok := keyFileMap["client_email"]; !ok {
		log.Fatal("Field client_email missing from service account keyfile")
	} else {
		if t, aok := saClientEmailIntfc.(string); !aok {
			log.Fatal("Field client_email not of type string")
		} else {
			saClientEmail = t
		}
	}

	logInfo("Storing gke credential %v on disk...", credentialsParam.Credentials)
	err = ioutil.WriteFile("/key-file.json", []byte(credential.AdditionalProperties.ServiceAccountKeyfile), 0600)
	if err != nil {
		log.Fatal("Failed writing service account keyfile: ", err)
	}

	logInfo("Authenticating to google cloud")
	runCommand("gcloud", []string{"auth", "activate-service-account", saClientEmail, "--key-file", "/key-file.json"})

	logInfo("Setting gcloud account")
	runCommand("gcloud", []string{"config", "set", "account", saClientEmail})

	logInfo("Setting gcloud project")
	runCommand("gcloud", []string{"config", "set", "project", credential.AdditionalProperties.Project})

	logInfo("Getting gke credentials for cluster %v", credential.AdditionalProperties.Cluster)
	clustersGetCredentialsArsgs := []string{"container", "clusters", "get-credentials", credential.AdditionalProperties.Cluster}
	if credential.AdditionalProperties.Zone != "" {
		clustersGetCredentialsArsgs = append(clustersGetCredentialsArsgs, "--zone", credential.AdditionalProperties.Zone)
	} else if credential.AdditionalProperties.Region != "" {
		clustersGetCredentialsArsgs = append(clustersGetCredentialsArsgs, "--region", credential.AdditionalProperties.Region)
	} else {
		log.Fatal("Credentials have no zone or region; at least one of them has to be defined")
	}
	runCommand("gcloud", clustersGetCredentialsArsgs)

	kubectlApplyArgs := []string{"apply", "-f", "/kubernetes.yaml", "-n", templateData.Namespace}
	if tmpl != nil {
		// always perform a dryrun to ensure we're not ending up in a semi broken state where half of the templates is successfully applied and others not
		logInfo("Performing a dryrun to test the validity of the manifests...")
		runCommand("kubectl", append(kubectlApplyArgs, "--dry-run"))
	}

	if !params.DryRun {

		// ensure that from now on any error runs the troubleshooting assistant
		assistTroubleshootingOnError = true
		paramsForTroubleshooting = params

		if tmpl != nil {
			patchServiceIfRequired(params, templateData.Name, templateData.Namespace)

			logInfo("Applying the manifests for real...")
			runCommand("kubectl", kubectlApplyArgs)

			logInfo("Waiting for the deployment to finish...")
			runCommand("kubectl", []string{"rollout", "status", "deployment", templateData.NameWithTrack, "-n", templateData.Namespace})
		}

		// clean up old stuff
		switch params.Action {
		case "deploy-canary":
			scaleCanaryDeployment(templateData.Name, templateData.Namespace, 1)
			deleteConfigsForParamsChange(params, fmt.Sprintf("%v-canary", templateData.Name), templateData.Namespace)
			deleteSecretsForParamsChange(params, fmt.Sprintf("%v-canary", templateData.Name), templateData.Namespace)
			break
		case "deploy-stable":
			scaleCanaryDeployment(templateData.Name, templateData.Namespace, 0)
			deleteResourcesForTypeSwitch(templateData.Name, templateData.Namespace)
			deleteConfigsForParamsChange(params, fmt.Sprintf("%v-stable", templateData.Name), templateData.Namespace)
			deleteSecretsForParamsChange(params, fmt.Sprintf("%v-stable", templateData.Name), templateData.Namespace)
			deleteIngressForVisibilityChange(params, templateData.Name, templateData.Namespace)
			removeEstafetteCloudflareAnnotations(params, templateData.Name, templateData.Namespace)
			break
		case "rollback-canary":
			scaleCanaryDeployment(templateData.Name, templateData.Namespace, 0)
			break
		case "deploy-simple":
			deleteResourcesForTypeSwitch(fmt.Sprintf("%v-canary", templateData.Name), templateData.Namespace)
			deleteResourcesForTypeSwitch(fmt.Sprintf("%v-stable", templateData.Name), templateData.Namespace)
			deleteConfigsForParamsChange(params, templateData.Name, templateData.Namespace)
			deleteSecretsForParamsChange(params, templateData.Name, templateData.Namespace)
			deleteIngressForVisibilityChange(params, templateData.Name, templateData.Namespace)
			removeEstafetteCloudflareAnnotations(params, templateData.Name, templateData.Namespace)
			break
		}

		assistTroubleshooting()
	}
}

func assistTroubleshooting() {
	if assistTroubleshootingOnError {
		logInfo("Showing current ingresses, services, configmaps, secrets, deployments ,poddisruptionbudgets, horizontalpodautoscalers, pods, endpoints for app=%v...", paramsForTroubleshooting.App)
		runCommandExtended("kubectl", []string{"get", "ing,svc,cm,secret,deploy,pdb,hpa,po,ep", "-l", fmt.Sprintf("app=%v", paramsForTroubleshooting.App), "-n", paramsForTroubleshooting.Namespace})

		if paramsForTroubleshooting.Action == "deploy-canary" {
			logInfo("Showing logs for canary deployment...")
			runCommandExtended("kubectl", []string{"logs", "-l", fmt.Sprintf("app=%v,track=canary", paramsForTroubleshooting.App), "-n", paramsForTroubleshooting.Namespace, "-c", paramsForTroubleshooting.App})
		}

		logInfo("Showing kubernetes events with the word %v in it...", paramsForTroubleshooting.App)
		c1 := exec.Command("kubectl", "get", "events", "--sort-by=.metadata.creationTimestamp", "-n", paramsForTroubleshooting.Namespace)
		c2 := exec.Command("grep", paramsForTroubleshooting.App)

		r, w := io.Pipe()
		c1.Stdout = w
		c2.Stdin = r

		var b2 bytes.Buffer
		c2.Stdout = &b2

		c1.Start()
		c2.Start()
		c1.Wait()
		w.Close()
		c2.Wait()
		io.Copy(os.Stdout, &b2)
	}
}

func scaleCanaryDeployment(name, namespace string, replicas int) {
	logInfo("Scaling canary deployment to %v replicas...", replicas)
	runCommand("kubectl", []string{"scale", "deploy", fmt.Sprintf("%v-canary", name), "-n", namespace, fmt.Sprintf("--replicas=%v", replicas)})
}

func deleteResourcesForTypeSwitch(name, namespace string) {
	// clean up resources in case a switch from simple to canary releases or vice versa has been made
	logInfo("Deleting simple type deployment, configmap, secret, hpa and pdb...")
	runCommand("kubectl", []string{"delete", "deploy", name, "-n", namespace, "--ignore-not-found=true"})
	runCommand("kubectl", []string{"delete", "configmap", fmt.Sprintf("%v-configs", name), "-n", namespace, "--ignore-not-found=true"})
	runCommand("kubectl", []string{"delete", "secret", fmt.Sprintf("%v-secrets", name), "-n", namespace, "--ignore-not-found=true"})
	runCommand("kubectl", []string{"delete", "hpa", name, "-n", namespace, "--ignore-not-found=true"})
	runCommand("kubectl", []string{"delete", "pdb", name, "-n", namespace, "--ignore-not-found=true"})
}

func deleteConfigsForParamsChange(params Params, name, namespace string) {
	if len(params.Configs.Files) == 0 {
		logInfo("Deleting application configs if it exists, because no configs are specified...")
		runCommand("kubectl", []string{"delete", "configmap", fmt.Sprintf("%v-configs", name), "-n", namespace, "--ignore-not-found=true"})
	}
}

func deleteSecretsForParamsChange(params Params, name, namespace string) {
	if len(params.Secrets.Keys) == 0 {
		logInfo("Deleting application secrets if it exists, because no secrets are specified...")
		runCommand("kubectl", []string{"delete", "secret", fmt.Sprintf("%v-secrets", name), "-n", namespace, "--ignore-not-found=true"})
	}
}

func deleteIngressForVisibilityChange(params Params, name, namespace string) {
	if params.Visibility == "public" {
		// public uses service of type loadbalancer and doesn't need ingress
		logInfo("Deleting ingress if it exists, which is used for visibility private or iap...")
		runCommand("kubectl", []string{"delete", "ingress", name, "-n", namespace, "--ignore-not-found=true"})
	}
}

func patchServiceIfRequired(params Params, name, namespace string) {
	if params.Visibility == "private" {
		serviceType, err := getCommandOutput("kubectl", []string{"get", "service", name, "-n", namespace, "-o=jsonpath={.spec.type}"})
		if err != nil {
			logInfo("Failed retrieving service type: %v", err)
		}
		if serviceType == "NodePort" || serviceType == "LoadBalancer" {
			logInfo("Service is of type %v, patching it...", serviceType)

			// brute force patch the service
			err = runCommandExtended("kubectl", []string{"patch", "service", name, "-n", namespace, "--type", "json", "--patch", "[{\"op\": \"remove\", \"path\": \"/spec/loadBalancerSourceRanges\"},{\"op\": \"remove\", \"path\": \"/spec/externalTrafficPolicy\"}, {\"op\": \"remove\", \"path\": \"/spec/ports/0/nodePort\"}, {\"op\": \"remove\", \"path\": \"/spec/ports/1/nodePort\"}, {\"op\": \"replace\", \"path\": \"/spec/type\", \"value\": \"ClusterIP\"}]"})
			if err != nil {
				err = runCommandExtended("kubectl", []string{"patch", "service", name, "-n", namespace, "--type", "json", "--patch", "[{\"op\": \"remove\", \"path\": \"/spec/externalTrafficPolicy\"}, {\"op\": \"remove\", \"path\": \"/spec/ports/0/nodePort\"}, {\"op\": \"remove\", \"path\": \"/spec/ports/1/nodePort\"}, {\"op\": \"replace\", \"path\": \"/spec/type\", \"value\": \"ClusterIP\"}]"})
			}
			if err != nil {
				log.Fatal(fmt.Sprintf("Failed patching service to change from %v to ClusterIP: ", serviceType), err)
			}
		} else {
			logInfo("Service is of type %v, no need to patch it", serviceType)
		}
	}
}

func removeEstafetteCloudflareAnnotations(params Params, name, namespace string) {
	if params.Visibility == "private" || params.Visibility == "iap" {
		// ingress is used and has the estafette.io/cloudflare annotations, so they should be removed from the service
		logInfo("Removing estafette.io/cloudflare annotations on the service if they exists, since they're now set on the ingress instead...")
		runCommand("kubectl", []string{"annotate", "svc", name, "-n", namespace, "estafette.io/cloudflare-dns-"})
		runCommand("kubectl", []string{"annotate", "svc", name, "-n", namespace, "estafette.io/cloudflare-proxy-"})
		runCommand("kubectl", []string{"annotate", "svc", name, "-n", namespace, "estafette.io/cloudflare-hostnames-"})
		runCommand("kubectl", []string{"annotate", "svc", name, "-n", namespace, "estafette.io/cloudflare-state-"})
	}
}

func handleError(err error) {
	if err != nil {
		assistTroubleshooting()
		log.Fatal(err)
	}
}

func runCommand(command string, args []string) {
	err := runCommandExtended(command, args)
	handleError(err)
}

func runCommandExtended(command string, args []string) error {
	logInfo("Running command '%v %v'...", command, strings.Join(args, " "))
	cmd := exec.Command(command, args...)
	cmd.Dir = "/estafette-work"
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	return err
}

func getCommandOutput(command string, args []string) (string, error) {
	logInfo("Running command '%v %v'...", command, strings.Join(args, " "))
	output, err := exec.Command(command, args...).Output()

	return string(output), err
}

func logInfo(message string, args ...interface{}) {
	formattedMessage := fmt.Sprintf(message, args...)
	log.Printf("\n%v\n\n", formattedMessage)
}
