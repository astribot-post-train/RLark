package sync

import (
	"context"
	"fmt"
	"time"

	"github.com/uptrace/bun"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	rlarkv1alpha1 "github.com/rlinf/rlark/api/rlark.io/v1alpha1"
)

// OrphanReconciler repairs deletion records missed while a controller was offline.
type OrphanReconciler struct {
	Reader client.Reader
	DB     *bun.DB
}

type orphanResource struct {
	table     string
	newObject func() client.Object
}

type orphanRow struct {
	ID              string    `bun:"id"`
	Namespace       string    `bun:"namespace"`
	Name            string    `bun:"name"`
	UID             string    `bun:"uid"`
	ResourceVersion string    `bun:"resource_version"`
	DeletionTime    time.Time `bun:"deletion_time"`
}

func (r *OrphanReconciler) NeedLeaderElection() bool { return true }

func (r *OrphanReconciler) Start(ctx context.Context) error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		if err := r.reconcile(ctx); err != nil && ctx.Err() == nil {
			log.FromContext(ctx).Error(err, "failed to reconcile orphaned deletion records")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (r *OrphanReconciler) reconcile(ctx context.Context) error {
	resources := []orphanResource{
		{"jobs", func() client.Object { return &rlarkv1alpha1.Job{} }},
		{"tasks", func() client.Object { return &rlarkv1alpha1.Task{} }},
		{"nodes", func() client.Object { return &rlarkv1alpha1.Node{} }},
		{"workflows", func() client.Object { return &rlarkv1alpha1.Workflow{} }},
	}
	for _, resource := range resources {
		var rows []orphanRow
		err := r.DB.NewSelect().TableExpr("?", bun.Ident(resource.table)).
			Column("id", "namespace", "name", "uid").
			ColumnExpr("raw #>> '{metadata,resourceVersion}' AS resource_version").
			ColumnExpr("(raw #>> '{metadata,deletionTimestamp}')::timestamptz AS deletion_time").
			Where("deleted_at IS NULL AND raw #>> '{metadata,deletionTimestamp}' IS NOT NULL").Scan(ctx, &rows)
		if err != nil {
			return fmt.Errorf("list orphan candidates in %s: %w", resource.table, err)
		}
		for _, row := range rows {
			obj := resource.newObject()
			err := r.Reader.Get(ctx, types.NamespacedName{Namespace: row.Namespace, Name: row.Name}, obj)
			orphan, checkErr := isOrphan(row.UID, obj, err)
			if checkErr != nil {
				return fmt.Errorf("check %s %s/%s: %w", resource.table, row.Namespace, row.Name, err)
			}
			if !orphan {
				continue
			}
			if err := r.markDeleted(ctx, resource.table, row); err != nil {
				return err
			}
		}
	}
	return nil
}

func isOrphan(uid string, obj client.Object, err error) (bool, error) {
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return string(obj.GetUID()) != uid, nil
}

func (r *OrphanReconciler) markDeleted(ctx context.Context, table string, row orphanRow) error {
	return r.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		for _, target := range []string{table, "latest_" + table} {
			_, err := tx.NewUpdate().TableExpr("?", bun.Ident(target)).
				Set("deleted_at = ?", row.DeletionTime).
				Where("namespace = ? AND name = ? AND uid = ?", row.Namespace, row.Name, row.UID).
				Where("deleted_at IS NULL AND raw #>> '{metadata,resourceVersion}' = ?", row.ResourceVersion).
				Where("raw #>> '{metadata,deletionTimestamp}' IS NOT NULL").Exec(ctx)
			if err != nil {
				return fmt.Errorf("mark orphan in %s: %w", target, err)
			}
		}
		return nil
	})
}
