package nstemplateset

import (
	"context"
	"fmt"
	"sort"

	rbac "k8s.io/api/rbac/v1"

	toolchainv1alpha1 "github.com/codeready-toolchain/api/api/v1alpha1"
	"github.com/codeready-toolchain/toolchain-common/pkg/configuration"
	"github.com/codeready-toolchain/toolchain-common/pkg/template"

	"github.com/redhat-cop/operator-utils/pkg/util"
	corev1 "k8s.io/api/core/v1"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type namespacesManager struct {
	*statusManager
}

// ensure ensures that all expected namespaces exists and they contain all the expected resources
// return `true, nil` when something changed, `false, nil` or `false, err` otherwise
func (r *namespacesManager) ensure(ctx context.Context, nsTmplSet *toolchainv1alpha1.NSTemplateSet) (createdOrUpdated bool, err error) {
	logger := log.FromContext(ctx)
	logger.Info("ensuring namespaces", "tier", nsTmplSet.Spec.TierName)
	spacename := nsTmplSet.GetName()
	userNamespaces, err := fetchNamespacesByOwner(ctx, r.Client, spacename)
	if err != nil {
		return false, r.wrapErrorWithStatusUpdate(ctx, nsTmplSet, r.setStatusProvisionFailed, err, "failed to list namespaces with label owner '%s'", spacename)
	}

	tierTemplatesByType, err := r.getTierTemplatesForAllNamespaces(ctx, nsTmplSet)
	if err != nil {
		return false, r.wrapErrorWithStatusUpdate(ctx, nsTmplSet, r.setStatusNamespaceProvisionFailed, err,
			"failed to get TierTemplates for tier '%s'", nsTmplSet.Spec.TierName)
	}
	toDeprovision, found := nextNamespaceToDeprovision(tierTemplatesByType, userNamespaces)
	if found {
		if err := r.setStatusUpdatingIfNotProvisioning(ctx, nsTmplSet); err != nil {
			return false, err
		}
		if err := r.Client.Delete(ctx, toDeprovision); err != nil {
			return false, r.wrapErrorWithStatusUpdate(ctx, nsTmplSet, r.setStatusUpdateFailed, err, "failed to delete namespace %s", toDeprovision.Name)
		}
		logger.Info("deleted namespace as part of NSTemplateSet update", "namespace", toDeprovision.Name)
		return true, nil // we deleted the namespace - wait for another reconcile
	}

	// find next namespace for provisioning namespace resource
	tierTemplate, userNamespace, found, err := r.nextNamespaceToProvisionOrUpdate(ctx, tierTemplatesByType, userNamespaces)
	if err != nil {
		return false, err
	}
	if !found {
		logger.Info("no more namespaces to create", "spacename", nsTmplSet.GetName())
		return false, nil
	}

	if len(userNamespaces) > 0 {
		if err := r.setStatusUpdatingIfNotProvisioning(ctx, nsTmplSet); err != nil {
			return false, err
		}
	} else {
		if err := r.setStatusProvisioningIfNotUpdating(ctx, nsTmplSet); err != nil {
			return false, err
		}
	}
	// create namespace resource
	return true, r.ensureNamespace(ctx, nsTmplSet, tierTemplate, userNamespace)
}

// ensureNamespace ensures that the namespace exists and that it contains all the expected resources
func (r *namespacesManager) ensureNamespace(ctx context.Context, nsTmplSet *toolchainv1alpha1.NSTemplateSet, tierTemplate *tierTemplate, userNamespace *corev1.Namespace) error {
	logger := log.FromContext(ctx)
	logger.Info("ensuring namespace", "namespace", tierTemplate.typeName, "tier", nsTmplSet.Spec.TierName)

	createOrUpdateNamespace := false
	if userNamespace == nil {
		// userNamespace does not exist, need to create the namespace
		createOrUpdateNamespace = true
		logger.Info("namespace needs to be created")
	} else {
		// userNamespace exists, check if the namespace needs to be updated
		upToDate, err := r.namespaceHasExpectedLabelsFromTemplate(tierTemplate, userNamespace)
		if err != nil {
			return r.wrapErrorWithStatusUpdate(ctx, nsTmplSet, r.setStatusNamespaceProvisionFailed, err, "failed to get namespace object from template for namespace type '%s'", tierTemplate.typeName)
		}
		createOrUpdateNamespace = !upToDate
		logger.Info("namespace needs to be updated", "namespace", userNamespace.Name)
	}

	// create namespace before creating inner resources because creating the namespace may take some time
	if createOrUpdateNamespace {
		return r.ensureNamespaceResource(ctx, nsTmplSet, tierTemplate)
	}
	return r.ensureInnerNamespaceResources(ctx, nsTmplSet, tierTemplate, userNamespace)
}

// namespaceHasExpectedLabelsFromTemplate checks if the namespace has the expected labels from the template object
// note: checks only if the namespace has labels that match the provided template, it does not check whether any labels could have been removed
func (r *namespacesManager) namespaceHasExpectedLabelsFromTemplate(tierTemplate *tierTemplate, userNamespace *corev1.Namespace) (bool, error) {
	objs, err := tierTemplate.process(r.Scheme, map[string]string{
		SpaceName: userNamespace.GetLabels()[toolchainv1alpha1.SpaceLabelKey],
	}, template.RetainNamespaces)
	if err != nil {
		return false, err
	}

	var tmplObj runtimeclient.Object
	for _, object := range objs {
		if object.GetName() == userNamespace.Name {
			tmplObj = object
		}
	}

	if tmplObj == nil {
		return false, fmt.Errorf("no matching template object found for namespace %s", userNamespace.Name)
	}

	if !mapContains(userNamespace.GetLabels(), tmplObj.GetLabels()) {
		return false, nil
	}

	return true, nil
}

func mapContains(actual, contains map[string]string) bool {
	if contains == nil {
		return true // contains has no values
	} else if actual == nil {
		return false // actual has no values and contains has values
	}

	for containsKey, containsValue := range contains {
		v, ok := actual[containsKey]
		if !ok {
			return false
		}
		if v != containsValue {
			return false
		}
	}
	return true
}

// ensureNamespaceResource ensures that the namespace exists.
func (r *namespacesManager) ensureNamespaceResource(ctx context.Context, nsTmplSet *toolchainv1alpha1.NSTemplateSet, tierTemplate *tierTemplate) error {
	logger := log.FromContext(ctx)
	logger.Info("creating namespace", "spacename", nsTmplSet.GetName(), "tier", nsTmplSet.Spec.TierName, "type", tierTemplate.typeName)
	objs, err := tierTemplate.process(r.Scheme, map[string]string{
		SpaceName: nsTmplSet.GetName(),
	}, template.RetainNamespaces)
	if err != nil {
		return r.wrapErrorWithStatusUpdate(ctx, nsTmplSet, r.setStatusNamespaceProvisionFailed, err, "failed to process template for namespace type '%s'", tierTemplate.typeName)
	}

	labels := map[string]string{
		toolchainv1alpha1.SpaceLabelKey:    nsTmplSet.GetName(),
		toolchainv1alpha1.TypeLabelKey:     tierTemplate.typeName,
		toolchainv1alpha1.ProviderLabelKey: toolchainv1alpha1.ProviderLabelValue,
	}

	// Note: we don't see an owner reference between the NSTemplateSet (namespaced resource) and the namespace (cluster-wide resource)
	// because a namespaced resource cannot be the owner of a cluster resource (the GC will delete the child resource, considering it is an orphan resource)
	// As a consequence, when the NSTemplateSet is deleted, we explicitly delete the associated namespaces that belong to the same user.
	// see https://issues.redhat.com/browse/CRT-429

	_, err = r.ApplyToolchainObjects(ctx, objs, labels)
	if err != nil {
		return r.wrapErrorWithStatusUpdate(ctx, nsTmplSet, r.setStatusNamespaceProvisionFailed, err, "failed to create namespace with type '%s'", tierTemplate.typeName)
	}
	logger.Info("namespace provisioned", "namespace", tierTemplate)
	return nil
}

// ensureInnerNamespaceResources ensure that the namespace has the expected resources.
func (r *namespacesManager) ensureInnerNamespaceResources(ctx context.Context, nsTmplSet *toolchainv1alpha1.NSTemplateSet, tierTemplate *tierTemplate, namespace *corev1.Namespace) error {
	logger := log.FromContext(ctx)
	logger.Info("ensuring namespace resources", "spacename", nsTmplSet.GetName(), "tier", nsTmplSet.Spec.TierName, "type", tierTemplate.typeName)
	nsName := namespace.GetName()
	newObjs, err := tierTemplate.process(r.Scheme, map[string]string{
		SpaceName: nsTmplSet.GetName(),
	}, template.RetainAllButNamespaces)
	if err != nil {
		return r.wrapErrorWithStatusUpdate(ctx, nsTmplSet, r.setStatusNamespaceProvisionFailed, err, "failed to process template for namespace '%s'", nsName)
	}

	if currentRef, exists := namespace.Labels[toolchainv1alpha1.TemplateRefLabelKey]; exists && currentRef != "" && currentRef != tierTemplate.templateRef {
		logger.Info("checking obsolete namespace resources", "spacename", nsTmplSet.GetName(), "tier", nsTmplSet.Spec.TierName, "type", tierTemplate.typeName)
		if err := r.setStatusUpdatingIfNotProvisioning(ctx, nsTmplSet); err != nil {
			return err
		}
		currentTierTemplate, err := getTierTemplate(ctx, r.GetHostClusterClient, currentRef)
		if err != nil {
			return r.wrapErrorWithStatusUpdate(ctx, nsTmplSet, r.setStatusUpdateFailed, err, "failed to retrieve current TierTemplate with name '%s'", currentRef)
		}
		currentObjs, err := currentTierTemplate.process(r.Scheme, map[string]string{
			SpaceName: nsTmplSet.GetName(),
		}, template.RetainAllButNamespaces)
		if err != nil {
			return r.wrapErrorWithStatusUpdate(ctx, nsTmplSet, r.setStatusUpdateFailed, err, "failed to process template for TierTemplate with name '%s'", currentRef)
		}
		if err := deleteObsoleteObjects(ctx, r.Client, currentObjs, newObjs); err != nil {
			return r.wrapErrorWithStatusUpdate(ctx, nsTmplSet, r.setStatusUpdateFailed, err, "failed to delete redundant objects in namespace '%s'", nsName)
		}
	}

	var labels = map[string]string{
		toolchainv1alpha1.ProviderLabelKey: toolchainv1alpha1.ProviderLabelValue,
		toolchainv1alpha1.SpaceLabelKey:    nsTmplSet.GetName(),
	}
	if _, err = r.ApplyToolchainObjects(ctx, newObjs, labels); err != nil {
		return r.wrapErrorWithStatusUpdate(ctx, nsTmplSet, r.setStatusNamespaceProvisionFailed, err, "failed to provision namespace '%s' with required resources", nsName)
	}

	if namespace.Labels == nil {
		namespace.Labels = make(map[string]string)
	}

	// Adding label indicating that the namespace is up-to-date with TierTemplate
	namespace.Labels[toolchainv1alpha1.TemplateRefLabelKey] = tierTemplate.templateRef
	namespace.Labels[toolchainv1alpha1.TierLabelKey] = tierTemplate.tierName
	if err := r.Client.Update(ctx, namespace); err != nil {
		return r.wrapErrorWithStatusUpdate(ctx, nsTmplSet, r.setStatusNamespaceProvisionFailed, err, "failed to update namespace '%s'", nsName)
	}

	logger.Info("namespace provisioned with all required resources", "templateRef", tierTemplate.templateRef)

	// TODO add validation for other objects
	return nil // nothing changed, no error occurred
}

// ensureDeleted ensures that the namespaces that are owned by the space (based on the label) are deleted.
// The method deletes only one namespace in one call.
// It returns true if all the namespaces are gone and returns false if we should re-try:
//
//	If there is no namespaces found then it returns true, nil.
//	If there is still some namespace which is not already in terminating state then it triggers
//	   the deletion of the namespace (one namespace in one call) and returns false, nil
//	If a namespace deletion was triggered previously but is not complete yet (namespace is in terminating state)
//	   then it returns false, nil.
//
// If some error happened then it returns false, error
func (r *namespacesManager) ensureDeleted(ctx context.Context, nsTmplSet *toolchainv1alpha1.NSTemplateSet) (bool, error) {
	// now, we can delete all "child" namespaces explicitly
	spacename := nsTmplSet.Name
	userNamespaces, err := fetchNamespacesByOwner(ctx, r.Client, spacename)
	if err != nil {
		return false, r.wrapErrorWithStatusUpdate(ctx, nsTmplSet, r.setStatusTerminatingFailed, err, "failed to list namespace with label owner '%s'", spacename)
	}

	if len(userNamespaces) == 0 {
		return true, nil // All namespaces are gone
	}
	ns := userNamespaces[0]
	if !util.IsBeingDeleted(&ns) {
		log.FromContext(ctx).Info("deleting a user namespace associated with the deleted NSTemplateSet", "namespace", ns.Name)
		if err := r.Client.Delete(ctx, &ns); err != nil {
			return false, r.wrapErrorWithStatusUpdate(ctx, nsTmplSet, r.setStatusTerminatingFailed, err, "failed to delete user namespace '%s'", ns.Name)
		}
		return false, nil // The namespace deletion is triggered so we should stop here. When the namespace is actually deleted the reconcile will be triggered again
	}
	// implies namespace has a deletion timestamp but has not been deleted yet, let's wait until is gone - it will trigger another reconcile
	return false, nil
}

func (r *namespacesManager) getTierTemplatesForAllNamespaces(ctx context.Context, nsTmplSet *toolchainv1alpha1.NSTemplateSet) ([]*tierTemplate, error) {
	var tmpls []*tierTemplate
	for _, ns := range nsTmplSet.Spec.Namespaces {
		nsTmpl, err := getTierTemplate(ctx, r.GetHostClusterClient, ns.TemplateRef)
		if err != nil {
			return nil, err
		}
		tmpls = append(tmpls, nsTmpl)
	}
	return tmpls, nil
}

// fetchNamespacesByOwner returns all current namespaces belonging to the given space
// i.e., labeled with `"toolchain.dev.openshift.com/owner":<spacename>`
func fetchNamespacesByOwner(ctx context.Context, cl runtimeclient.Client, spacename string) ([]corev1.Namespace, error) {
	// fetch all namespace with owner=spacename label
	userNamespaceList := &corev1.NamespaceList{}
	if err := cl.List(ctx, userNamespaceList, listBySpaceLabel(spacename)); err != nil {
		return nil, err
	}
	// sort namespaces by name
	sort.Slice(userNamespaceList.Items, func(i, j int) bool {
		return userNamespaceList.Items[i].Name < userNamespaceList.Items[j].Name
	})
	return userNamespaceList.Items, nil
}

// nextNamespaceToProvisionOrUpdate returns first namespace (from given namespaces) whose status is active and
// either revision is not set or revision or tier doesn't equal to the current one.
// It also returns namespace present in tcNamespaces but not found in given namespaces
func (r *namespacesManager) nextNamespaceToProvisionOrUpdate(ctx context.Context, tierTemplatesByType []*tierTemplate, namespaces []corev1.Namespace) (*tierTemplate, *corev1.Namespace, bool, error) {
	for _, nsTemplate := range tierTemplatesByType {
		namespace, found := findNamespace(namespaces, nsTemplate.typeName)
		if found {
			if namespace.Status.Phase == corev1.NamespaceActive {
				isProvisioned, err := r.isUpToDateAndProvisioned(ctx, &namespace, nsTemplate)
				if err != nil {
					return nsTemplate, nil, true, err
				}
				if !isProvisioned {
					return nsTemplate, &namespace, true, nil
				}
			}
		} else {
			return nsTemplate, nil, true, nil
		}
	}
	return nil, nil, false, nil
}

// nextNamespaceToDeprovision returns namespace (and information of it was found) that should be deprovisioned
// because its type wasn't found in the set of namespace types in NSTemplateSet
func nextNamespaceToDeprovision(tierTemplatesByType []*tierTemplate, namespaces []corev1.Namespace) (*corev1.Namespace, bool) {
Namespaces:
	for _, ns := range namespaces {
		for _, nsTemplate := range tierTemplatesByType {
			if nsTemplate.typeName == ns.Labels[toolchainv1alpha1.TypeLabelKey] {
				continue Namespaces
			}
		}
		return &ns, true
	}
	return nil, false
}

func findNamespace(namespaces []corev1.Namespace, typeName string) (corev1.Namespace, bool) {
	for _, ns := range namespaces {
		if ns.Labels[toolchainv1alpha1.TypeLabelKey] == typeName {
			return ns, true
		}
	}
	return corev1.Namespace{}, false
}

func getNamespaceName(request reconcile.Request) (string, error) {
	namespace := request.Namespace
	if namespace == "" {
		return configuration.GetWatchNamespace()
	}
	return namespace, nil
}

// isUpToDateAndProvisioned checks if the obj has the correct Template Reference Label.
// If so, it processes the tier template to get the expected roles and rolebindings and then checks if they are actually present in the namespace.
func (r *namespacesManager) isUpToDateAndProvisioned(ctx context.Context, ns *corev1.Namespace, tierTemplate *tierTemplate) (bool, error) {
	logger := log.FromContext(ctx)
	logger.Info("checking if namespace is up-to-date and provisioned", "namespace_name", ns.Name, "namespace_labels", ns.Labels, "tier_name", tierTemplate.tierName)
	if ns.GetLabels() != nil &&
		ns.GetLabels()[toolchainv1alpha1.TierLabelKey] == tierTemplate.tierName &&
		ns.GetLabels()[toolchainv1alpha1.TemplateRefLabelKey] == tierTemplate.templateRef {

		newObjs, err := tierTemplate.process(r.Scheme, map[string]string{
			Username:  ns.GetLabels()[toolchainv1alpha1.SpaceLabelKey],
			SpaceName: ns.GetLabels()[toolchainv1alpha1.SpaceLabelKey], // both username and space name are required here, since rolebindings are still created with the USERNAME param.
		}, template.RetainAllButNamespaces)
		if err != nil {
			return false, err
		}
		processedRoles := []runtimeclient.Object{}
		processedRoleBindings := []runtimeclient.Object{}
		for _, obj := range newObjs {
			switch obj.GetObjectKind().GroupVersionKind().Kind {
			case "Role":
				processedRoles = append(processedRoles, obj)
			case "RoleBinding":
				processedRoleBindings = append(processedRoleBindings, obj)
			}
		}

		// get the space name from namespace
		spacename, exists := ns.GetLabels()[toolchainv1alpha1.SpaceLabelKey]
		if !exists {
			return false, fmt.Errorf("namespace doesn't have space label")
		}
		roleList := rbac.RoleList{}
		rolebindingList := rbac.RoleBindingList{}
		if err = r.AllNamespacesClient.List(ctx, &roleList, runtimeclient.InNamespace(ns.GetName())); err != nil {
			return false, err
		}
		if err = r.AllNamespacesClient.List(ctx, &rolebindingList, runtimeclient.InNamespace(ns.GetName())); err != nil {
			return false, err
		}

		// check the names of the roles and roleBindings as well
		for _, role := range processedRoles {
			if found, err := r.containsRole(roleList.Items, role, spacename); !found || err != nil {
				return false, err
			}
		}

		for _, rolebinding := range processedRoleBindings {
			if found, err := r.containsRoleBindings(rolebindingList.Items, rolebinding, spacename); !found || err != nil {
				return false, err
			}
		}
		logger.Info("namespace is up-to-date and provisioned", "namespace_name", ns.Name, "namespace_labels", ns.Labels, "tier_name", tierTemplate.tierName)
		return true, nil
	}
	logger.Info("namespace is not up-to-date or not provisioned", "namespace_name", ns.Name, "namespace_labels", ns.Labels, "tier_name", tierTemplate.tierName)
	return false, nil
}

func (r *namespacesManager) containsRole(list []rbac.Role, obj runtimeclient.Object, spacename string) (bool, error) {
	if obj.GetObjectKind().GroupVersionKind().Kind != "Role" {
		return false, fmt.Errorf("object is not a role")
	}
	for _, val := range list {
		if val.GetName() == obj.GetName() {
			// check if space label exists
			if spaceValue, exists := val.GetLabels()[toolchainv1alpha1.SpaceLabelKey]; !exists || spaceValue != spacename {
				return false, nil
			}
			return true, nil
		}
	}
	return false, nil
}

func (r *namespacesManager) containsRoleBindings(list []rbac.RoleBinding, obj runtimeclient.Object, spacename string) (bool, error) {
	if obj.GetObjectKind().GroupVersionKind().Kind != "RoleBinding" {
		return false, fmt.Errorf("object is not a rolebinding")
	}
	for _, val := range list {
		if val.GetName() == obj.GetName() {
			// check if owner label exists
			if spaceValue, exists := val.GetLabels()[toolchainv1alpha1.SpaceLabelKey]; !exists || spaceValue != spacename {
				return false, nil
			}

			return true, nil
		}
	}
	return false, nil
}

func (r *namespacesManager) setProvisionedNamespaceList(ctx context.Context, nsTmplSet *toolchainv1alpha1.NSTemplateSet) (err error) {
	logger := log.FromContext(ctx)
	logger.Info("setting provisioned namespaces", "spacename", nsTmplSet.GetName())
	spacename := nsTmplSet.GetName()
	userNamespaces, err := fetchNamespacesByOwner(ctx, r.Client, spacename)
	if err != nil {
		return err
	}
	return r.updateStatusProvisionedNamespaces(ctx, nsTmplSet, userNamespaces)
}
