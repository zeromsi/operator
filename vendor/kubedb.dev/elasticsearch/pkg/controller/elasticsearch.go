/*
Copyright AppsCode Inc. and Contributors

Licensed under the PolyForm Noncommercial License 1.0.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://github.com/appscode/licenses/raw/1.0.0/PolyForm-Noncommercial-1.0.0.md

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	api "kubedb.dev/apimachinery/apis/kubedb/v1alpha1"
	"kubedb.dev/apimachinery/client/clientset/versioned/typed/kubedb/v1alpha1/util"
	"kubedb.dev/apimachinery/pkg/eventer"
	validator "kubedb.dev/elasticsearch/pkg/admission"
	"kubedb.dev/elasticsearch/pkg/distribution"

	"github.com/appscode/go/log"
	"github.com/pkg/errors"
	core "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	kutil "kmodules.xyz/client-go"
	core_util "kmodules.xyz/client-go/core/v1"
	dynamic_util "kmodules.xyz/client-go/dynamic"
	meta_util "kmodules.xyz/client-go/meta"
	"kmodules.xyz/client-go/tools/queue"
)

func (c *Controller) create(elasticsearch *api.Elasticsearch) error {
	if err := validator.ValidateElasticsearch(c.Client, c.ExtClient, elasticsearch, true); err != nil {
		c.recorder.Event(
			elasticsearch,
			core.EventTypeWarning,
			eventer.EventReasonInvalid,
			err.Error(),
		)
		log.Errorln(err)
		return nil
	}

	if elasticsearch.Status.Phase == "" {
		es, err := util.UpdateElasticsearchStatus(
			context.TODO(),
			c.ExtClient.KubedbV1alpha1(),
			elasticsearch.ObjectMeta,
			func(in *api.ElasticsearchStatus) *api.ElasticsearchStatus {
				in.Phase = api.DatabasePhaseCreating
				return in
			},
			metav1.UpdateOptions{},
		)
		if err != nil {
			return err
		}
		elasticsearch.Status = es.Status
	}

	// create Governing Service
	if err := c.ensureElasticGvrSvc(elasticsearch); err != nil {
		return fmt.Errorf(`failed to create governing Service for "%v/%v". Reason: %v`, elasticsearch.Namespace, elasticsearch.Name, err)
	}

	// ensure database Service
	vt1, err := c.ensureService(elasticsearch)
	if err != nil {
		return err
	}

	// ensure database StatefulSet
	elasticsearch, vt2, err := c.ensureElasticsearchNode(elasticsearch)
	if err != nil {
		return err
	}

	// If both err==nil & elasticsearch == nil,
	// the object was dropped from the work-queue, to process later.
	// return nil.
	if elasticsearch == nil {
		return nil
	}

	if vt1 == kutil.VerbCreated && vt2 == kutil.VerbCreated {
		c.recorder.Event(
			elasticsearch,
			core.EventTypeNormal,
			eventer.EventReasonSuccessful,
			"Successfully created Elasticsearch",
		)
	} else if vt1 == kutil.VerbPatched || vt2 == kutil.VerbPatched {
		c.recorder.Event(
			elasticsearch,
			core.EventTypeNormal,
			eventer.EventReasonSuccessful,
			"Successfully patched Elasticsearch",
		)
	}

	// ensure appbinding before ensuring Restic scheduler and restore
	_, err = c.ensureAppBinding(elasticsearch)
	if err != nil {
		log.Errorln(err)
		return err
	}

	if _, err := meta_util.GetString(elasticsearch.Annotations, api.AnnotationInitialized); err == kutil.ErrNotFound &&
		elasticsearch.Spec.Init != nil && elasticsearch.Spec.Init.StashRestoreSession != nil {

		if elasticsearch.Status.Phase == api.DatabasePhaseInitializing {
			return nil
		}

		// add phase that database is being initialized
		mg, err := util.UpdateElasticsearchStatus(
			context.TODO(), c.ExtClient.KubedbV1alpha1(),
			elasticsearch.ObjectMeta,
			func(in *api.ElasticsearchStatus) *api.ElasticsearchStatus {
				in.Phase = api.DatabasePhaseInitializing
				return in
			},
			metav1.UpdateOptions{},
		)
		if err != nil {
			return err
		}
		elasticsearch.Status = mg.Status

		init := elasticsearch.Spec.Init
		if init.StashRestoreSession != nil {
			log.Debugf("Elasticsearch %v/%v is waiting for restoreSession to be succeeded", elasticsearch.Namespace, elasticsearch.Name)
			return nil
		}
	}

	es, err := util.UpdateElasticsearchStatus(
		context.TODO(),
		c.ExtClient.KubedbV1alpha1(),
		elasticsearch.ObjectMeta,
		func(in *api.ElasticsearchStatus) *api.ElasticsearchStatus {
			in.Phase = api.DatabasePhaseRunning
			in.ObservedGeneration = elasticsearch.Generation
			return in
		},
		metav1.UpdateOptions{},
	)
	if err != nil {
		return err
	}
	elasticsearch.Status = es.Status

	// ensure StatsService for desired monitoring
	if _, err := c.ensureStatsService(elasticsearch); err != nil {
		c.recorder.Eventf(
			elasticsearch,
			core.EventTypeWarning,
			eventer.EventReasonFailedToCreate,
			"Failed to manage monitoring system. Reason: %v",
			err,
		)
		log.Errorln(err)
		return nil
	}

	if err := c.manageMonitor(elasticsearch); err != nil {
		c.recorder.Eventf(
			elasticsearch,
			core.EventTypeWarning,
			eventer.EventReasonFailedToCreate,
			"Failed to manage monitoring system. Reason: %v",
			err,
		)
		log.Errorf("failed to manage monitoring system. Reason: %v", err)
		return nil
	}

	return nil
}

func (c *Controller) ensureElasticsearchNode(es *api.Elasticsearch) (*api.Elasticsearch, kutil.VerbType, error) {
	if es == nil {
		return nil, kutil.VerbUnchanged, errors.New("Elasticsearch object is empty")
	}

	elastic, err := distribution.NewElasticsearch(c.Client, c.ExtClient, es)
	if err != nil {
		return nil, kutil.VerbUnchanged, errors.Wrap(err, "failed to get elasticsearch distribution")
	}

	if err = elastic.EnsureCertSecrets(); err != nil {
		return nil, kutil.VerbUnchanged, errors.Wrap(err, "failed to ensure certificates secret")
	}

	if err = elastic.EnsureDatabaseSecret(); err != nil {
		return nil, kutil.VerbUnchanged, errors.Wrap(err, "failed to ensure database credential secret")
	}

	if !elastic.IsAllRequiredSecretAvailable() {
		log.Warningf("Required secrets for Elasticsearch: %s/%s are not ready yet", es.Namespace, es.Name)
		queue.EnqueueAfter(c.esQueue.GetQueue(), elastic.UpdatedElasticsearch(), 5*time.Second)
		return nil, kutil.VerbUnchanged, nil
	}

	if err = elastic.EnsureDefaultConfig(); err != nil {
		return nil, kutil.VerbUnchanged, errors.Wrap(err, "failed to ensure default configuration for elasticsearch")
	}

	// Ensure Service account, role, rolebinding, and PSP for database statefulsets
	if err := c.ensureDatabaseRBAC(elastic.UpdatedElasticsearch()); err != nil {
		return nil, kutil.VerbUnchanged, errors.Wrap(err, "failed to create RBAC role or roleBinding")
	}

	vt := kutil.VerbUnchanged
	topology := elastic.UpdatedElasticsearch().Spec.Topology
	if topology != nil {
		vt1, err := elastic.EnsureClientNodes()
		if err != nil {
			return nil, kutil.VerbUnchanged, err
		}
		vt2, err := elastic.EnsureMasterNodes()
		if err != nil {
			return nil, kutil.VerbUnchanged, err
		}
		vt3, err := elastic.EnsureDataNodes()
		if err != nil {
			return nil, kutil.VerbUnchanged, err
		}

		if vt1 == kutil.VerbCreated && vt2 == kutil.VerbCreated && vt3 == kutil.VerbCreated {
			vt = kutil.VerbCreated
		} else if vt1 == kutil.VerbPatched || vt2 == kutil.VerbPatched || vt3 == kutil.VerbPatched {
			vt = kutil.VerbPatched
		}
	} else {
		vt, err = elastic.EnsureCombinedNode()
		if err != nil {
			return nil, kutil.VerbUnchanged, err
		}
	}

	// Need some time to build elasticsearch cluster. Nodes will communicate with each other
	time.Sleep(time.Second * 30)

	return elastic.UpdatedElasticsearch(), vt, nil
}

func (c *Controller) halt(db *api.Elasticsearch) error {
	if db.Spec.Halted && db.Spec.TerminationPolicy != api.TerminationPolicyHalt {
		return errors.New("can't halt db. 'spec.terminationPolicy' is not 'Halt'")
	}
	log.Infof("Halting Elasticsearch %v/%v", db.Namespace, db.Name)
	if err := c.haltDatabase(db); err != nil {
		return err
	}
	if err := c.waitUntilPaused(db); err != nil {
		return err
	}
	log.Infof("update status of Elasticsearch %v/%v to Halted.", db.Namespace, db.Name)
	if _, err := util.UpdateElasticsearchStatus(
		context.TODO(),
		c.ExtClient.KubedbV1alpha1(),
		db.ObjectMeta,
		func(in *api.ElasticsearchStatus) *api.ElasticsearchStatus {
			in.Phase = api.DatabasePhaseHalted
			in.ObservedGeneration = db.Generation
			return in
		},
		metav1.UpdateOptions{},
	); err != nil {
		return err
	}
	return nil
}

func (c *Controller) terminate(elasticsearch *api.Elasticsearch) error {
	owner := metav1.NewControllerRef(elasticsearch, api.SchemeGroupVersion.WithKind(api.ResourceKindElasticsearch))

	// If TerminationPolicy is "halt", keep PVCs,Secrets intact.
	// TerminationPolicyPause is deprecated and will be removed in future.
	if elasticsearch.Spec.TerminationPolicy == api.TerminationPolicyHalt || elasticsearch.Spec.TerminationPolicy == api.TerminationPolicyPause {
		if err := c.removeOwnerReferenceFromOffshoots(elasticsearch); err != nil {
			return err
		}
	} else {
		// If TerminationPolicy is "wipeOut", delete everything (ie, PVCs,Secrets,Snapshots).
		// If TerminationPolicy is "delete", delete PVCs and keep snapshots,secrets intact.
		// In both these cases, don't create dormantdatabase
		if err := c.setOwnerReferenceToOffshoots(elasticsearch, owner); err != nil {
			return err
		}
	}

	if elasticsearch.Spec.Monitor != nil {
		if err := c.deleteMonitor(elasticsearch); err != nil {
			log.Errorln(err)
			return nil
		}
	}
	return nil
}

func (c *Controller) setOwnerReferenceToOffshoots(elasticsearch *api.Elasticsearch, owner *metav1.OwnerReference) error {
	selector := labels.SelectorFromSet(elasticsearch.OffshootSelectors())

	// If TerminationPolicy is "wipeOut", delete snapshots and secrets,
	// else, keep it intact.
	if elasticsearch.Spec.TerminationPolicy == api.TerminationPolicyWipeOut {
		if err := c.wipeOutDatabase(elasticsearch.ObjectMeta, elasticsearch.Spec.GetSecrets(), owner); err != nil {
			return errors.Wrap(err, "error in wiping out database.")
		}
	} else {
		// Make sure secret's ownerreference is removed.
		if err := dynamic_util.RemoveOwnerReferenceForItems(
			context.TODO(),
			c.DynamicClient,
			core.SchemeGroupVersion.WithResource("secrets"),
			elasticsearch.Namespace,
			elasticsearch.Spec.GetSecrets(),
			elasticsearch); err != nil {
			return err
		}
	}
	// delete PVC for both "wipeOut" and "delete" TerminationPolicy.
	return dynamic_util.EnsureOwnerReferenceForSelector(
		context.TODO(),
		c.DynamicClient,
		core.SchemeGroupVersion.WithResource("persistentvolumeclaims"),
		elasticsearch.Namespace,
		selector,
		owner)
}

func (c *Controller) removeOwnerReferenceFromOffshoots(elasticsearch *api.Elasticsearch) error {
	// First, Get LabelSelector for Other Components
	labelSelector := labels.SelectorFromSet(elasticsearch.OffshootSelectors())

	if err := dynamic_util.RemoveOwnerReferenceForSelector(
		context.TODO(),
		c.DynamicClient,
		core.SchemeGroupVersion.WithResource("persistentvolumeclaims"),
		elasticsearch.Namespace,
		labelSelector,
		elasticsearch); err != nil {
		return err
	}
	if err := dynamic_util.RemoveOwnerReferenceForItems(
		context.TODO(),
		c.DynamicClient,
		core.SchemeGroupVersion.WithResource("secrets"),
		elasticsearch.Namespace,
		elasticsearch.Spec.GetSecrets(),
		elasticsearch); err != nil {
		return err
	}
	return nil
}

func (c *Controller) GetDatabase(meta metav1.ObjectMeta) (runtime.Object, error) {
	elasticsearch, err := c.esLister.Elasticsearches(meta.Namespace).Get(meta.Name)
	if err != nil {
		return nil, err
	}

	return elasticsearch, nil
}

func (c *Controller) SetDatabaseStatus(meta metav1.ObjectMeta, phase api.DatabasePhase, reason string) error {
	elasticsearch, err := c.esLister.Elasticsearches(meta.Namespace).Get(meta.Name)
	if err != nil {
		return err
	}
	_, err = util.UpdateElasticsearchStatus(context.TODO(), c.ExtClient.KubedbV1alpha1(), elasticsearch.ObjectMeta, func(in *api.ElasticsearchStatus) *api.ElasticsearchStatus {
		in.Phase = phase
		in.Reason = reason
		return in
	}, metav1.UpdateOptions{})
	return err
}

func (c *Controller) UpsertDatabaseAnnotation(meta metav1.ObjectMeta, annotation map[string]string) error {
	elasticsearch, err := c.esLister.Elasticsearches(meta.Namespace).Get(meta.Name)
	if err != nil {
		return err
	}

	_, _, err = util.PatchElasticsearch(context.TODO(), c.ExtClient.KubedbV1alpha1(), elasticsearch, func(in *api.Elasticsearch) *api.Elasticsearch {
		in.Annotations = core_util.UpsertMap(in.Annotations, annotation)
		return in
	}, metav1.PatchOptions{})
	return err
}
