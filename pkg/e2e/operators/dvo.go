package operators

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/onsi/ginkgo/v2"
	operatorv1 "github.com/operator-framework/api/pkg/operators/v1"
	operatorv1alpha "github.com/operator-framework/api/pkg/operators/v1alpha1"

	"github.com/openshift/osde2e/pkg/common/alert"
	"github.com/openshift/osde2e/pkg/common/config"
	"github.com/openshift/osde2e/pkg/common/helper"
	"github.com/openshift/osde2e/pkg/common/util"

	. "github.com/onsi/gomega"
	viper "github.com/openshift/osde2e/pkg/common/concurrentviper"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

var deploymentValidationOperatorTestName string = "[Suite: informing] [OSD] Deployment Validation Operator (dvo)"

func init() {
	alert.RegisterGinkgoAlert(deploymentValidationOperatorTestName, "SD-SREP", "Ron Green", "sd-cicd-alerts", "sd-cicd@redhat.com", 4)
}

var _ = ginkgo.Describe(deploymentValidationOperatorTestName, func() {
	const (
		operatorNamespace         = "osde2e-deployment-validation-operator"
		operatorName              = "deployment-validation-operator"
		operatorDeploymentName    = "deployment-validation-operator"
		operatorCsvDisplayName    = "Deployment Validation Operator"
		fVMinimum3Replicas        = `(?i)deployment_validation_operator_minimum_three_replicas\b\{\w+.\".*?\"\,.*?\,\w+.\".*?\".*?\"(?P<name>.*?)\".*?\"}`
		fVNoLivenessProbe         = `(?i)deployment_validation_operator_no_liveness_probe\b\{\w+.\".*?\"\,.*?\,\w+.\".*?\".*?\"(?P<name>.*?)\".*?\"}`
		fVNoReadinessProbe        = `(?i)deployment_validation_operator_no_readiness_probe\b\{\w+.\".*?\"\,.*?\,\w+.\".*?\".*?\"(?P<name>.*?)\".*?\"}`
		fVNoReadOnlyFs            = `(?i)deployment_validation_operator_no_read_only_root_fs\b\{\w+.\".*?\"\,.*?\,\w+.\".*?\".*?\"(?P<name>.*?)\".*?\"}`
		fVRequiredAnnotationEmail = `(?i)deployment_validation_operator_required_annotation_email\b\{\w+.\".*?\"\,.*?\,\w+.\".*?\".*?\"(?P<name>.*?)\".*?\"}`
		fVRequiredLabelOwner      = `(?i)deployment_validation_operator_required_label_owner\b\{\w+.\".*?\"\,.*?\,\w+.\".*?\".*?\"(?P<name>.*?)\".*?\"}`
		fVRunAsNonRoot            = `(?i)deployment_validation_operator_run_as_non_root\b\{\w+.\".*?\"\,.*?\,\w+.\".*?\".*?\"(?P<name>.*?)\".*?\"}`
		fVUnsetCPURequirements    = `(?i)deployment_validation_operator_unset_cpu_requirements\b\{\w+.\".*?\"\,.*?\,\w+.\".*?\".*?\"(?P<name>.*?)\".*?\"}`
		fVUnsetMemoryRequirements = `(?i)deployment_validation_operator_unset_memory_requirements\b\{\w+.\".*?\"\,.*?\,\w+.\".*?\".*?\"(?P<name>.*?)\".*?\"}`

		operatorLockFile = "deployment-validation-operator-lock"

		defaultDesiredReplicas int32 = 1
	)

	var clusterRoles = []string{
		"deployment-validation-operator-admin",
		"deployment-validation-operator-edit",
		"deployment-validation-operator-view",
	}

	h := helper.New()
	nodeLabels := make(map[string]string)

	//Create DVO project and install operator
	deployDVO(helper.New(),
		operatorNamespace,
		operatorName,
		operatorName,
		"deployment-validation-operator")

	// Future test once new versions are in place for DVO
	//checkUpgrade(helper.New(),
	//	operatorNamespace,
	//	operatorName,
	//	operatorName,
	//	"deployment-validation-operator-registry")

	checkClusterServiceVersion(h, operatorNamespace, operatorCsvDisplayName)
	checkConfigMapLockfile(h, operatorNamespace, operatorLockFile)
	checkDeployment(h, operatorNamespace, operatorDeploymentName, defaultDesiredReplicas)
	checkClusterRoles(h, clusterRoles, false)

	util.GinkgoIt("empty node-label deployment should get created", func() {
		// Set it to a wildcard dedicated-admin
		h.SetServiceAccount("system:serviceaccount:%s:cluster-admin")

		// Test creating a basic deployment
		ds := makeDeployment("dvo-test-case", h.GetNamespacedServiceAccount(), nodeLabels)
		_, err := h.Kube().AppsV1().Deployments(h.CurrentProject()).Create(context.TODO(), &ds, metav1.CreateOptions{})
		Expect(err).NotTo(HaveOccurred())

		wait.PollImmediate(2*time.Second, 15*time.Second, func() (bool, error) {

			resp := h.Kube().CoreV1().Services(operatorNamespace).ProxyGet("http", "deployment-validation-operator-metrics", "8383", "/metrics", nil)
			data, _ := resp.DoRaw(context.TODO())
			// Check for now if 503 repeat, if 200 continue (DVO-37 will fix the 3 pod metric issue)
			if strings.Contains(string(data), "\"code\":503") {
				return false, nil
			}

			// Setup array of regex filters for future check
			var dvoMetricCheck [9]string
			dvoMetricCheck[0] = fVMinimum3Replicas
			dvoMetricCheck[1] = fVNoLivenessProbe
			dvoMetricCheck[2] = fVNoReadinessProbe
			dvoMetricCheck[3] = fVNoReadOnlyFs
			dvoMetricCheck[4] = fVRequiredAnnotationEmail
			dvoMetricCheck[5] = fVRequiredLabelOwner
			dvoMetricCheck[6] = fVRunAsNonRoot
			dvoMetricCheck[7] = fVUnsetCPURequirements
			dvoMetricCheck[8] = fVUnsetMemoryRequirements

			// Cast metric data to string
			dataString := string(data)

			// Check if corresponding DVO Metric exists against deployment
			for _, dvoCheck := range dvoMetricCheck {
				if !(regexDVOCheck(dvoCheck, dataString, ds.Name)) {
					return false, nil
				}
			}
			return true, nil
		})

	}, float64(viper.GetFloat64(config.Tests.PollingTimeout)))

	// Teardown DVO (Project, Operator Group, and Subscription)
	deleteDVO(helper.New(),
		operatorNamespace,
		operatorName,
		operatorName,
		"deployment-validation-operator")
})

// Function to create a standard deployment
func makeDeployment(name, sa string, nodeLabels map[string]string) appsv1.Deployment {
	matchLabels := make(map[string]string)
	matchLabels["name"] = name
	dep := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%s-%s", name, util.RandomStr(5)),
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: matchLabels,
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Name:   name,
					Labels: matchLabels,
				},
				Spec: v1.PodSpec{
					NodeSelector:       nodeLabels,
					ServiceAccountName: sa,
					Containers: []v1.Container{
						{
							Name:  "test",
							Image: "registry.access.redhat.com/ubi8/ubi-minimal",
						},
					},
				},
			},
		},
	}

	return dep
}

// Helper function to perform regex substring checks
func regexDVOCheck(filterValue string, data string, deploymentName string) bool {
	r, _ := regexp.Compile(filterValue)
	match := r.FindAllStringSubmatch(data, -1)
	for _, v := range match {
		if strings.Contains(v[1], deploymentName) {
			return true
		}
	}
	return false
}

// Create DVO Subscription
func deployDVO(h *helper.H, subNamespace string, subName string, packageName string, regServiceName string) {

	ginkgo.Context("Install DVO", func() {
		util.GinkgoIt("should install DVO for future tests", func() {

			//Setup vars for error and target namespace for Operator Group
			var err error
			var targetns = []string{
				"osde2e-deployment-validation-operator",
			}

			// Create DVO Namespace
			h.CreateProject(subName)

			// Create Operator Group
			_, err = h.Operator().OperatorsV1().OperatorGroups(subNamespace).Create(context.TODO(), &operatorv1.OperatorGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      subName,
					Namespace: subNamespace,
				},
				Spec: operatorv1.OperatorGroupSpec{
					TargetNamespaces: targetns,
				},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("failed trying to create Operator Group %s", subName))

			// Create Subscription
			_, err = h.Operator().OperatorsV1alpha1().Subscriptions(subNamespace).Create(context.TODO(), &operatorv1alpha.Subscription{
				ObjectMeta: metav1.ObjectMeta{
					Name:      subName,
					Namespace: subNamespace,
				},
				Spec: &operatorv1alpha.SubscriptionSpec{
					Package:                "deployment-validation-operator",
					Channel:                "alpha",
					CatalogSourceNamespace: "openshift-marketplace",
					CatalogSource:          "community-operators",
					InstallPlanApproval:    operatorv1alpha.ApprovalAutomatic,
				},
			}, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("failed trying to create Subscription %s", subName))

			log.Printf("Created DVO subscription %s", subName)

		}, float64(viper.GetFloat64(config.Tests.PollingTimeout)))
	})
}

// Delete DVO Subscription
func deleteDVO(h *helper.H, subNamespace string, subName string, packageName string, regServiceName string) {

	ginkgo.Context("Operator Upgrade", func() {
		util.GinkgoIt("should upgrade from the replaced version", func() {

			// Get the CSV we're currently installed with
			var latestCSV string
			var sub *operatorv1alpha.Subscription
			var err error

			pollErr := wait.PollImmediate(5*time.Second, 5*time.Minute, func() (bool, error) {
				sub, err = h.Operator().OperatorsV1alpha1().Subscriptions(subNamespace).Get(context.TODO(), subName, metav1.GetOptions{})
				if err != nil {
					return false, err
				}
				latestCSV = sub.Status.CurrentCSV
				if latestCSV != "" {
					return true, nil
				}
				return false, nil
			})
			Expect(pollErr).NotTo(HaveOccurred(), fmt.Sprintf("failed trying to get Subscription %s in %s namespace: %v", subName, subNamespace, err))

			// Delete current Operator installation
			err = h.Operator().OperatorsV1().OperatorGroups(subNamespace).Delete(context.TODO(), subName, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("failed trying to delete operator group %s", subName))
			log.Printf("Removed operator group %s", subName)

			err = h.Operator().OperatorsV1alpha1().Subscriptions(subNamespace).Delete(context.TODO(), subName, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("failed trying to delete Subscription %s", subName))
			log.Printf("Removed subscription %s", subName)

			err = h.Operator().OperatorsV1alpha1().ClusterServiceVersions(subNamespace).Delete(context.TODO(), latestCSV, metav1.DeleteOptions{})
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("failed trying to delete ClusterServiceVersion %s", latestCSV))
			log.Printf("Removed csv %s", latestCSV)

			err = helper.DeleteNamespace(subNamespace, true, h)
			Expect(err).NotTo(HaveOccurred(), fmt.Sprintf("failed trying to delete project %s", subNamespace))
			log.Printf("Removed project %s", subNamespace)

			Eventually(func() bool {
				_, err := h.Operator().OperatorsV1alpha1().InstallPlans(subNamespace).Get(context.TODO(), sub.Status.Install.Name, metav1.GetOptions{})
				return apierrors.IsNotFound(err)
			}, 5*time.Minute, 10*time.Second).Should(BeTrue(), "installplan never garbage collected")
			log.Printf("Verified installplan removal")
		}, float64(viper.GetFloat64(config.Tests.PollingTimeout)))
	})
}
