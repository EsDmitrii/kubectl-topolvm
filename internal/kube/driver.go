package kube

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/EsDmitrii/kubectl-topolvm/internal/topolvm"
)

// DetectDriver picks the TopoLVM driver: the flag when given, else whichever CSIDriver exists.
func DetectDriver(ctx context.Context, core kubernetes.Interface, flag string) (topolvm.Driver, []string, error) {
	if flag != "" {
		d, err := topolvm.ParseDriver(flag)
		return d, nil, err
	}
	for _, d := range []topolvm.Driver{topolvm.Current, topolvm.Legacy} {
		_, err := core.StorageV1().CSIDrivers().Get(ctx, d.Name, metav1.GetOptions{})
		switch {
		case err == nil:
			return d, nil, nil
		case apierrors.IsForbidden(err):
			return topolvm.Current, []string{"cannot get csidrivers: forbidden; assuming topolvm.io"}, nil
		case !apierrors.IsNotFound(err):
			return topolvm.Driver{}, nil, fmt.Errorf("get csidriver %s: %w", d.Name, err)
		}
	}
	return topolvm.Driver{}, nil, fmt.Errorf("no TopoLVM CSIDriver found (looked for %s and %s); use --driver",
		topolvm.Current.Name, topolvm.Legacy.Name)
}
