package memberoperatorconfig

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/codeready-toolchain/member-operator/pkg/apis"
	"github.com/codeready-toolchain/member-operator/pkg/webhook/deploy"
	commonconfig "github.com/codeready-toolchain/toolchain-common/pkg/configuration"
	membercfg "github.com/codeready-toolchain/toolchain-common/pkg/configuration/memberoperatorconfig"
	"github.com/codeready-toolchain/toolchain-common/pkg/test"
	testconfig "github.com/codeready-toolchain/toolchain-common/pkg/test/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	appsv1 "k8s.io/api/apps/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestReconcileWhenMemberOperatorConfigIsAvailable(t *testing.T) {
	// given
	config := commonconfig.NewMemberOperatorConfigWithReset(t, testconfig.MemberStatus().RefreshPeriod("10s"))
	controller, cl := prepareReconcile(t, config)

	// when
	_, err := controller.Reconcile(context.TODO(), newRequest())

	// then
	require.NoError(t, err)
	actual, err := membercfg.GetConfiguration(test.NewFakeClient(t))
	require.NoError(t, err)
	assert.Equal(t, 10*time.Second, actual.MemberStatus().RefreshPeriod())

	t.Run("update with new version", func(t *testing.T) {
		// given
		refreshPeriod := "8s"
		config.Spec.MemberStatus.RefreshPeriod = &refreshPeriod
		err := cl.Update(context.TODO(), config)
		require.NoError(t, err)

		// when
		_, err = controller.Reconcile(context.TODO(), newRequest())

		// then
		require.NoError(t, err)
		actual, err := membercfg.GetConfiguration(test.NewFakeClient(t))
		require.NoError(t, err)
		assert.Equal(t, 8*time.Second, actual.MemberStatus().RefreshPeriod())
	})
}

func TestReconcileWhenGetConfigurationReturnsError(t *testing.T) {
	// given
	cl := test.NewFakeClient(t)
	cl.MockGet = func(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		return fmt.Errorf("get error")
	}
	controller := Reconciler{
		Client: cl,
		Log:    ctrl.Log.WithName("controllers").WithName("MemberOperatorConfig"),
	}

	// when
	_, err := controller.Reconcile(context.TODO(), newRequest())

	// then
	require.EqualError(t, err, "get error")
	actual, err := membercfg.GetConfiguration(test.NewFakeClient(t))
	require.NoError(t, err)
	matchesDefaultConfig(t, actual)
}

func TestReconcileWhenListSecretsReturnsError(t *testing.T) {
	// given
	config := commonconfig.NewMemberOperatorConfigWithReset(t, testconfig.MemberStatus().RefreshPeriod("10s"))
	cl := test.NewFakeClient(t, config)
	cl.MockList = func(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
		return fmt.Errorf("list error")
	}
	controller := Reconciler{
		Client: cl,
		Log:    ctrl.Log.WithName("controllers").WithName("MemberOperatorConfig"),
	}

	// when
	_, err := controller.Reconcile(context.TODO(), newRequest())

	// then
	require.EqualError(t, err, "list error")
	actual, err := membercfg.GetConfiguration(test.NewFakeClient(t))
	require.NoError(t, err)
	matchesDefaultConfig(t, actual)
}

func TestReconcileWhenMemberOperatorConfigIsNotPresent(t *testing.T) {
	// given
	controller, _ := prepareReconcile(t)

	// when
	_, err := controller.Reconcile(context.TODO(), newRequest())

	// then
	require.NoError(t, err)
	actual, err := membercfg.GetConfiguration(test.NewFakeClient(t))
	require.NoError(t, err)
	matchesDefaultConfig(t, actual)
}

func TestHandleAutoscalerDeploy(t *testing.T) {
	t.Run("deploy false", func(t *testing.T) {
		// given
		config := commonconfig.NewMemberOperatorConfigWithReset(t, testconfig.Autoscaler().Deploy(false))
		controller, cl := prepareReconcile(t, config)
		actualConfig, err := membercfg.GetConfiguration(cl)
		require.NoError(t, err)
		ctx := log.IntoContext(context.TODO(), controller.Log)

		// when
		err = controller.handleAutoscalerDeploy(ctx, actualConfig, test.MemberOperatorNs)

		// then
		require.NoError(t, err)
		actualPrioClass := &schedulingv1.PriorityClass{}
		err = cl.Get(context.TODO(), test.NamespacedName("", "member-operator-autoscaling-buffer"), actualPrioClass)
		require.Error(t, err)
		require.True(t, errors.IsNotFound(err))
	})

	t.Run("deploy true", func(t *testing.T) {
		// given
		config := commonconfig.NewMemberOperatorConfigWithReset(t, testconfig.Autoscaler().Deploy(true))
		controller, cl := prepareReconcile(t, config)

		actualConfig, err := membercfg.GetConfiguration(cl)
		require.NoError(t, err)
		ctx := log.IntoContext(context.TODO(), controller.Log)

		// when
		err = controller.handleAutoscalerDeploy(ctx, actualConfig, test.MemberOperatorNs)

		// then
		require.NoError(t, err)
		actualPrioClass := &schedulingv1.PriorityClass{}
		err = cl.Get(context.TODO(), test.NamespacedName("", "member-operator-autoscaling-buffer"), actualPrioClass)
		require.NoError(t, err)

		t.Run("removed when set to back false", func(t *testing.T) {
			modifiedConfig := commonconfig.UpdateMemberOperatorConfigWithReset(t, cl, testconfig.Autoscaler().Deploy(false))
			err = cl.Update(context.TODO(), modifiedConfig)
			require.NoError(t, err)
			updatedConfig, err := membercfg.ForceLoadConfiguration(cl)
			require.NoError(t, err)
			require.False(t, updatedConfig.Autoscaler().Deploy())
			ctx := log.IntoContext(context.TODO(), controller.Log)

			// when
			err = controller.handleAutoscalerDeploy(ctx, updatedConfig, test.MemberOperatorNs)

			// then
			require.NoError(t, err)
			actualPrioClass := &schedulingv1.PriorityClass{}
			err = cl.Get(context.TODO(), test.NamespacedName("", "member-operator-autoscaling-buffer"), actualPrioClass)
			require.Error(t, err)
			require.True(t, errors.IsNotFound(err))
		})
	})

	t.Run("deploy error", func(t *testing.T) {
		// given
		config := commonconfig.NewMemberOperatorConfigWithReset(t, testconfig.Autoscaler().Deploy(true))
		controller, cl := prepareReconcile(t, config)
		actualConfig, err := membercfg.GetConfiguration(cl)
		require.NoError(t, err)
		ctx := log.IntoContext(context.TODO(), controller.Log)

		// when
		cl.(*test.FakeClient).MockGet = func(_ context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			return fmt.Errorf("client error")
		}
		err = controller.handleAutoscalerDeploy(ctx, actualConfig, test.MemberOperatorNs)

		// then
		require.Error(t, err)
		require.Contains(t, err.Error(), "cannot deploy autoscaling buffer template: ")
	})

	t.Run("delete error", func(t *testing.T) {
		// given
		config := commonconfig.NewMemberOperatorConfigWithReset(t, testconfig.Autoscaler().Deploy(false))
		actualPrioClass := &schedulingv1.PriorityClass{
			ObjectMeta: metav1.ObjectMeta{
				Name: "member-operator-autoscaling-buffer",
			},
		}
		controller, cl := prepareReconcile(t, config, actualPrioClass)
		actualConfig, err := membercfg.GetConfiguration(cl)
		require.NoError(t, err)
		ctx := log.IntoContext(context.TODO(), controller.Log)

		// when
		cl.(*test.FakeClient).MockDelete = func(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
			return fmt.Errorf("client error")
		}
		err = controller.handleAutoscalerDeploy(ctx, actualConfig, test.MemberOperatorNs)

		// then
		require.EqualError(t, err, "cannot delete autoscaling buffer object: client error")
	})
}

func TestHandleWebhookDeploy(t *testing.T) {
	t.Run("deployment not created when webhook deploy is false", func(t *testing.T) {
		// given
		config := commonconfig.NewMemberOperatorConfigWithReset(t, testconfig.Webhook().Deploy(false))
		controller, cl := prepareReconcile(t, config)

		actualConfig, err := membercfg.GetConfiguration(cl)
		require.NoError(t, err)
		ctx := log.IntoContext(context.TODO(), controller.Log)

		// when
		err = controller.handleWebhookDeploy(ctx, actualConfig, test.MemberOperatorNs)

		// then
		require.NoError(t, err)
		actualDeployment := &appsv1.Deployment{}
		err = cl.Get(context.TODO(), test.NamespacedName(test.MemberOperatorNs, "member-operator-webhook"), actualDeployment)
		require.Error(t, err)
		require.True(t, errors.IsNotFound(err))
	})

	t.Run("webhook deployment deleted when deploy is disabled", func(t *testing.T) {
		// given
		config := commonconfig.NewMemberOperatorConfigWithReset(t, testconfig.Webhook().Deploy(false))
		s := scheme.Scheme
		err := apis.AddToScheme(s)
		require.NoError(t, err)
		objs, err := deploy.GetTemplateObjects(s, test.MemberOperatorNs, "test/image", []byte("asdfasdfasdf"))
		initObjs := []client.Object{config}
		for _, obj := range objs {
			initObjs = append(initObjs, obj.DeepCopyObject().(client.Object))
		}
		require.NoError(t, err)
		controller, cl := prepareReconcile(t, initObjs...)

		actualConfig, err := membercfg.GetConfiguration(cl)
		require.NoError(t, err)
		ctx := log.IntoContext(context.TODO(), controller.Log)

		// when
		err = controller.handleWebhookDeploy(ctx, actualConfig, test.MemberOperatorNs)

		// then
		require.NoError(t, err)
		for _, obj := range objs {
			actualObject := &unstructured.Unstructured{}
			actualObject.SetGroupVersionKind(obj.GetObjectKind().GroupVersionKind())
			err = cl.Get(context.TODO(), test.NamespacedName(obj.GetNamespace(), obj.GetName()), actualObject)
			if _, found := obj.GetAnnotations()[deploy.WebhookDeploymentNoDeletionAnnotation]; found {
				// resource should not be deleted
				require.NoError(t, err)
			} else {
				// resource should be deleted
				require.Error(t, err)
				require.True(t, errors.IsNotFound(err))
			}
		}
	})

	t.Run("deployment created when webhook deploy is true", func(t *testing.T) {
		// given
		config := commonconfig.NewMemberOperatorConfigWithReset(t, testconfig.Webhook().Deploy(true))
		controller, cl := prepareReconcile(t, config)
		actualConfig, err := membercfg.GetConfiguration(cl)
		require.NoError(t, err)
		ctx := log.IntoContext(context.TODO(), controller.Log)

		// when
		err = controller.handleWebhookDeploy(ctx, actualConfig, test.MemberOperatorNs)

		// then
		require.NoError(t, err)
		actualDeployment := &appsv1.Deployment{}
		err = cl.Get(context.TODO(), test.NamespacedName(test.MemberOperatorNs, "member-operator-webhook"), actualDeployment)
		require.NoError(t, err)
	})

	t.Run("deployment error", func(t *testing.T) {
		// given
		config := commonconfig.NewMemberOperatorConfigWithReset(t, testconfig.Webhook().Deploy(true))
		controller, cl := prepareReconcile(t, config)
		actualConfig, err := membercfg.GetConfiguration(cl)
		require.NoError(t, err)
		ctx := log.IntoContext(context.TODO(), controller.Log)

		// when
		cl.(*test.FakeClient).MockGet = func(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			return fmt.Errorf("client error")
		}
		err = controller.handleWebhookDeploy(ctx, actualConfig, test.MemberOperatorNs)

		// then
		require.EqualError(t, err, "cannot deploy webhook template: client error")
	})
}

func prepareReconcile(t *testing.T, initObjs ...client.Object) (*Reconciler, client.Client) {
	os.Setenv("WATCH_NAMESPACE", test.MemberOperatorNs)
	restore := test.SetEnvVarAndRestore(t, "MEMBER_OPERATOR_WEBHOOK_IMAGE", "webhookimage")
	t.Cleanup(restore)
	s := scheme.Scheme
	err := apis.AddToScheme(s)
	require.NoError(t, err)
	fakeClient := test.NewFakeClient(t, initObjs...)
	r := &Reconciler{
		Client: fakeClient,
		Log:    ctrl.Log.WithName("controllers").WithName("MemberOperatorConfig"),
	}
	return r, fakeClient
}

func newRequest() reconcile.Request {
	return reconcile.Request{
		NamespacedName: test.NamespacedName(test.MemberOperatorNs, "config"),
	}
}

func matchesDefaultConfig(t *testing.T, actual membercfg.Configuration) {
	assert.Equal(t, 5*time.Second, actual.MemberStatus().RefreshPeriod())
}
