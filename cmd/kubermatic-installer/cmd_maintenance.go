/*
Copyright 2026 The Kubermatic Kubernetes Platform contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	kubermaticv1 "k8c.io/kubermatic/sdk/v2/apis/kubermatic/v1"
	"k8c.io/kubermatic/v2/pkg/install/maintenance"
	kubermaticmaster "k8c.io/kubermatic/v2/pkg/install/stack/kubermatic-master"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrlruntimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	gatewayclientscheme "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/scheme"
)

type MaintenanceOptions struct {
	Options

	Kubeconfig  string
	KubeContext string
	Namespace   string

	Title      string
	Message    string
	Until      string
	RetryAfter int
}

func MaintenanceCommand(logger *logrus.Logger) *cobra.Command {
	opt := MaintenanceOptions{}

	cmd := &cobra.Command{
		Use:   "maintenance",
		Short: "Enable or disable the maintenance mode of the KKP dashboard and API",
		Long: "Switches the dashboard and API host to a static maintenance page (HTTP 503) and back.\n" +
			"The page uses the dashboard branding from KubermaticConfiguration spec.ui.config.\n" +
			"Requires a KKP installation that uses the Gateway API with Envoy Gateway.",
		SilenceUsage: true,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			options.CopyInto(&opt.Options)
		},
	}

	cmd.PersistentFlags().StringVar(&opt.Kubeconfig, "kubeconfig", "", "full path to a kubeconfig with cluster-admin permissions for the master cluster")
	cmd.PersistentFlags().StringVar(&opt.KubeContext, "kube-context", "", "context to use from the given kubeconfig")
	cmd.PersistentFlags().StringVar(&opt.Namespace, "namespace", kubermaticmaster.KubermaticOperatorNamespace, "namespace of the Kubermatic installation")

	enable := &cobra.Command{
		Use:          "enable",
		Short:        "Show the maintenance page instead of the dashboard and API",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE:         handleErrors(logger, MaintenanceEnableFunc(logger, &opt)),
	}
	enable.Flags().StringVar(&opt.Title, "title", "", "headline of the maintenance page (default \"Scheduled maintenance\")")
	enable.Flags().StringVar(&opt.Message, "message", "", "text of the maintenance page")
	enable.Flags().StringVar(&opt.Until, "until", "", "expected end of the maintenance, shown as free-form text on the page")
	enable.Flags().IntVar(&opt.RetryAfter, "retry-after", 300, "value of the Retry-After response header in seconds")

	disable := &cobra.Command{
		Use:          "disable",
		Short:        "Serve the dashboard and API again",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE:         handleErrors(logger, MaintenanceDisableFunc(logger, &opt)),
	}

	status := &cobra.Command{
		Use:          "status",
		Short:        "Show whether the maintenance mode is enabled",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE:         handleErrors(logger, MaintenanceStatusFunc(logger, &opt)),
	}

	cmd.AddCommand(enable, disable, status)

	return cmd
}

func maintenanceClient(opt *MaintenanceOptions) (ctrlruntimeclient.Client, error) {
	client, _, err := maintenanceClients(opt)

	return client, err
}

func maintenanceClients(opt *MaintenanceOptions) (ctrlruntimeclient.Client, *rest.Config, error) {
	if opt.Kubeconfig == "" {
		return nil, nil, errors.New("no kubeconfig (--kubeconfig) given")
	}

	config, err := loadKubeconfig(opt.Kubeconfig, opt.KubeContext)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get config: %w", err)
	}

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		clientgoscheme.AddToScheme,
		kubermaticv1.AddToScheme,
		gatewayclientscheme.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			return nil, nil, fmt.Errorf("failed to setup scheme: %w", err)
		}
	}

	client, err := ctrlruntimeclient.New(config, ctrlruntimeclient.Options{Scheme: scheme})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create Kubernetes client: %w", err)
	}

	return client, config, nil
}

func MaintenanceEnableFunc(logger *logrus.Logger, opt *MaintenanceOptions) cobraFuncE {
	return func(cmd *cobra.Command, args []string) error {
		client, config, err := maintenanceClients(opt)
		if err != nil {
			return err
		}

		clientset, err := kubernetes.NewForConfig(config)
		if err != nil {
			return fmt.Errorf("failed to create Kubernetes clientset: %w", err)
		}

		ctx := context.Background()

		cfg, err := findKubermaticConfiguration(ctx, client, opt.Namespace)
		if err != nil {
			return fmt.Errorf("failed to find KubermaticConfiguration: %w", err)
		}

		err = maintenance.Enable(ctx, client, cfg, opt.Namespace, maintenance.Options{
			Page: maintenance.PageOptions{
				Title:   opt.Title,
				Message: opt.Message,
				Until:   opt.Until,
			},
			RetryAfter: opt.RetryAfter,
			// relative branding assets are loaded from the dashboard Service through the API server
			AssetFetcher: maintenance.ServiceProxyAssetFetcher(clientset.CoreV1().RESTClient(), opt.Namespace, "kubermatic-dashboard", 80),
			Warn:         logger.Warnf,
		})
		if err != nil {
			return err
		}

		logger.Info("🚧 Maintenance mode enabled.")

		return nil
	}
}

func MaintenanceDisableFunc(logger *logrus.Logger, opt *MaintenanceOptions) cobraFuncE {
	return func(cmd *cobra.Command, args []string) error {
		client, err := maintenanceClient(opt)
		if err != nil {
			return err
		}

		if err := maintenance.Disable(context.Background(), client, opt.Namespace); err != nil {
			return err
		}

		logger.Info("✅ Maintenance mode disabled.")

		return nil
	}
}

func MaintenanceStatusFunc(logger *logrus.Logger, opt *MaintenanceOptions) cobraFuncE {
	return func(cmd *cobra.Command, args []string) error {
		client, err := maintenanceClient(opt)
		if err != nil {
			return err
		}

		status, err := maintenance.GetStatus(context.Background(), client, opt.Namespace)
		if err != nil {
			return err
		}

		fmt.Println(status.Summary())

		return nil
	}
}
