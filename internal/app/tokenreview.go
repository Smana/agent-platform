// SPDX-License-Identifier: Apache-2.0

package app

import (
	"fmt"

	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/Smana/agent-platform/internal/authn"
)

// tokenReviewClient is how authenticators obtains the apiserver's TokenReview client. It is called
// only when a tokenreview issuer is configured, so a JWKS-only broker needs no kubeconfig here.
type tokenReviewClient func() (authn.TokenReviewer, error)

// clusterTokenReviews is the broker's tokenReviewClient: the in-cluster client's
// AuthenticationV1().TokenReviews(). It builds its own rest config because the manager (and so its
// config) is created after authenticators. The chart's `system:auth-delegator` binding on the
// broker's ServiceAccount is what makes the call succeed.
func clusterTokenReviews() (authn.TokenReviewer, error) {
	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("token review client: %w", err)
	}
	return cs.AuthenticationV1().TokenReviews(), nil
}
