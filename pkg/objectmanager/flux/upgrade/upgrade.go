package upgrade

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/conditions"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"helm.sh/helm/v3/pkg/chart/loader"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const (
	upgradeGateConditionType     = "UpgradeAllowed"
	upgradeGateReasonPassed      = "Passed"
	upgradeGateReasonBlocked     = "Blocked"
	upgradeGateReasonInProgress  = "Checking"
	upgradeGateInProgressMessage = "CRDs are being analyzed for breaking version removals"
)

func conditionChecking(obj client.Object) *metav1.Condition {
	return &metav1.Condition{
		Type:               upgradeGateConditionType,
		Status:             metav1.ConditionUnknown,
		ObservedGeneration: obj.GetGeneration(),
		Reason:             upgradeGateReasonInProgress,
		Message:            upgradeGateInProgressMessage,
	}
}

// Allowed creates a temporary OCIRepository (derived from ociRepoName) for the given chartVersion
// and checks whether upgrading to it is safe by comparing its CRDs against the respective live
// cluster CRDs. Returns an UpgradeAllowed condition to track the upgrade allowed status and an error for technical errors.
// The caller is responsible preventing an upgrade by exiting reconciliation on UpgradeAllowed.Status False and requeuing on UpgradeAllowed.Status Unknonw.
// UpgradeAllowed.Status Unknown indicates that the upgrade check waits for resources required to check if an upgrade is allowed.
// UpgradeAllowed.Status False indicates a blocked upgrade that requires user interaction where a user can either migrate resources to pass the upgrade gate or set the obj.spec.Version back to match the obj.Status.Version.
// UpgradeAllowed.Status True indicates a passed upgrade check where a caller can proceed with updating any managed resources to the requested version.
// Upgrades are blocked if a CRD version is removed that is still tracked as a stored version.
func Allowed(ctx context.Context, obj client.Object, cl client.Client, ociRepoName types.NamespacedName, chartVersion string) (*metav1.Condition, error) {
	var ociRepo sourcev1.OCIRepository
	if err := cl.Get(ctx, ociRepoName, &ociRepo); err != nil {
		return nil, fmt.Errorf("fetching OCIRepository %q: %w", ociRepoName, err)
	}
	tempName := types.NamespacedName{Name: ociRepoName.Name + "-upgrade-gate", Namespace: ociRepoName.Namespace}
	var tempRepo sourcev1.OCIRepository
	if err := cl.Get(ctx, tempName, &tempRepo); apierrors.IsNotFound(err) {
		tempRepo = sourcev1.OCIRepository{
			Name:      tempName.Name,
			Namespace: tempName.Namespace,
			Spec: sourcev1.OCIRepositorySpec{
				URL:            ociRepo.Spec.URL,
				Interval:       ociRepo.Spec.Interval,
				Reference:      &sourcev1.OCIRepositoryRef{Tag: chartVersion},
				LayerSelector:  ociRepo.Spec.LayerSelector,
				SecretRef:      ociRepo.Spec.SecretRef,
				CertSecretRef:  ociRepo.Spec.CertSecretRef,
				ProxySecretRef: ociRepo.Spec.ProxySecretRef,
			},
		}
		if err := cl.Create(ctx, &tempRepo); err != nil {
			return nil, fmt.Errorf("creating temporary OCIRepository for version %q: %w", chartVersion, err)
		}
		return conditionChecking(obj), nil
	} else if err != nil {
		return nil, fmt.Errorf("fetching temporary OCIRepository %q: %w", tempName, err)
	}

	if cond := conditions.Get(&tempRepo, fluxmeta.ReadyCondition); cond != nil && cond.Status == "False" {
		return conditionChecking(obj), nil
	}
	if !conditions.IsTrue(&tempRepo, fluxmeta.ReadyCondition) || tempRepo.Status.Artifact == nil {
		return conditionChecking(obj), nil
	}

	resp, err := http.Get(tempRepo.Status.Artifact.URL)
	if err != nil {
		return nil, fmt.Errorf("downloading chart artifact from %q: %w", tempRepo.Status.Artifact.URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading chart artifact from %q: unexpected status %s", tempRepo.Status.Artifact.URL, resp.Status)
	}
	chart, err := loader.LoadArchive(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("loading helm chart archive: %w", err)
	}
	upgradePairs := []crdUpgrade{}
	for _, crdObj := range chart.CRDObjects() {
		var chartCRD apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(crdObj.File.Data, &chartCRD); err != nil {
			return nil, fmt.Errorf("parsing CRD %q from chart: %w", crdObj.Name, err)
		}
		var liveCRD apiextensionsv1.CustomResourceDefinition
		if err := cl.Get(ctx, types.NamespacedName{Name: chartCRD.Name}, &liveCRD); err != nil {
			return nil, fmt.Errorf("fetching live CRD %q: %w", chartCRD.Name, err)
		}
		upgradePairs = append(upgradePairs, crdUpgrade{from: liveCRD, to: chartCRD})
	}
	if err := allowed(upgradePairs); err != nil {
		return &metav1.Condition{
			Type:               upgradeGateConditionType,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: obj.GetGeneration(),
			Reason:             upgradeGateReasonBlocked,
			Message:            err.Error(),
		}, nil
	}
	return &metav1.Condition{
		Type:               upgradeGateConditionType,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: obj.GetGeneration(),
		Reason:             upgradeGateReasonPassed,
		Message:            "No CRD version is dropped that is still tracked as stored version",
	}, nil
}

type crdUpgrade struct {
	from apiextensionsv1.CustomResourceDefinition
	to   apiextensionsv1.CustomResourceDefinition
}

// allowed returns an error if a CRD upgrade removes a version that is still tracked as stored version
func allowed(upgrades []crdUpgrade) error {
	for _, u := range upgrades {
		// check for each from.Version if it has been removed from to.Versions
		for _, fromVersion := range u.from.Spec.Versions {
			if !slices.ContainsFunc(u.to.Spec.Versions, func(v apiextensionsv1.CustomResourceDefinitionVersion) bool {
				return v.Name == fromVersion.Name
				// if a fromVersion has been removed, we need to further investigate if the live CRD still tracks this version as a storedVersion
			}) && slices.Contains(u.from.Status.StoredVersions, fromVersion.Name) {
				return fmt.Errorf("can't remove CRD %q version %q. Migrate all existing %q resources on your control plane and remove %q from Status.StoredVersions of CRD %q", u.from.Spec.Names.Kind, fromVersion.Name, fromVersion.Name, fromVersion.Name, u.from.Spec.Names.Kind)
			}
		}
	}
	return nil
}
