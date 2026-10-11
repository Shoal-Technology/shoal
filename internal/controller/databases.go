package controller

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/kaiserern/shoal/api/v1alpha1"
	"github.com/kaiserern/shoal/internal/database"
	"github.com/kaiserern/shoal/internal/objects"
	"github.com/kaiserern/shoal/internal/resource"
	"github.com/kaiserern/shoal/internal/store"
)

// A Database expands into four objects it owns (ADR 0016, section 3.2):
// <db>-data (Volume), <db> (Workload and Service) and <db>-credentials (Secret).
// Ownership is an OwnerClaim record per object (resource.OwnerKey), which the
// server reads to refuse user edits. Deleting the Database cascades to the
// objects; a Volume keeps its data according to its retention (Retain).
const (
	dbDataSuffix  = "-data"
	dbCredsSuffix = "-credentials"
	dbPort        = 5432
	dbUser        = "app"
	dbDataPath    = "/var/lib/postgresql/data"
	// dbPGData is a subdirectory of the volume: a freshly formatted volume
	// holds lost+found, and initdb refuses a non-empty data directory.
	dbPGData = dbDataPath + "/pgdata"
	// dbUID is the postgres user in the official image; the data directory
	// must belong to it.
	dbUID = 999
)

// errNotOwned means an object with a Database's name belongs to someone else.
var errNotOwned = errors.New("delete it or choose another name")

// expansion is the outcome of expanding one Database. A refusal fails it (its
// objects are left as they are). A hold leaves its phase as it is and shows the
// wait; the workload is not changed while a hold lasts. restoreState is the
// restore state to record, if any.
type expansion struct {
	refusal      string
	hold         string
	restoreState string
}

// syncDatabases expands every Database into its owned objects, cascades the
// deletion of objects whose Database is gone, and writes each Database's
// status. It then runs the Databases' backups and restores (dbbackup.go). It
// returns the errors it skipped over.
func (c *Controller) syncDatabases(ctx context.Context, s *state) []error {
	recs, err := c.st.List(ctx, v1alpha1.KindDatabase)
	if err != nil {
		return []error{err}
	}
	var errs []error
	dbs := map[string]*v1alpha1.Database{}
	apps := map[string]string{}
	for _, r := range recs {
		d, err := objects.Database(r)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		dbs[r.Name] = d
		apps[r.Name] = r.Labels[appLabel]
	}
	// Without every Database decoded, a missing one cannot be told from a
	// broken one: do not cascade.
	if len(errs) == 0 {
		errs = append(errs, c.cascadeOwned(ctx, s, dbs)...)
	}
	for _, name := range sortedKeys(dbs) {
		d := dbs[name]
		exp, err := c.expandDatabase(ctx, d, apps[name])
		if err != nil {
			errs = append(errs, fmt.Errorf("database %s: %w", name, err))
			continue
		}
		if err := c.writeDatabaseStatus(ctx, s, d, exp); err != nil {
			errs = append(errs, err)
		}
	}
	return append(errs, c.syncDatabaseJobs(ctx, s, dbs)...)
}

// appLabel is the label that names the app a Database belongs to. Owned objects
// copy it when the Database has it (ADR 0016, pass C).
const appLabel = "app"

// expandDatabase makes the owned objects of d match its spec. app is the
// Database's app label, or "". The error is for transient store failures; a
// refusal or a hold is in the expansion.
func (c *Controller) expandDatabase(ctx context.Context, d *v1alpha1.Database, app string) (expansion, error) {
	name := d.GetMetadata().GetName()
	spec := d.GetSpec()
	version, image, ok := database.Resolve(spec.GetEngine(), spec.GetVersion())
	if !ok {
		return expansion{refusal: fmt.Sprintf("version %q is not in the image table", spec.GetVersion())}, nil
	}
	cur := d.GetStatus().GetVersion()
	if cur != "" && database.Major(cur) != database.Major(version) {
		return expansion{refusal: fmt.Sprintf("major upgrade from %s to %s is refused: restore a backup into a new Database of version %s (ADR 0016, section 3.8)",
			cur, version, version)}, nil
	}

	// A restore creates a new Database (section 3.7). It is refused for a
	// Database that already exists, so a restore never overwrites live data.
	var exp expansion
	var source *v1alpha1.DatabaseBackupRecord
	if from := spec.GetRestoreFrom(); from != "" {
		src, refusal, err := c.restoreSource(ctx, from)
		if err != nil {
			return expansion{}, err
		}
		if refusal != "" {
			return expansion{refusal: refusal}, nil
		}
		fresh := d.GetStatus().GetPhase() == ""
		if !fresh && d.GetStatus().GetRestoreState() == "" {
			return expansion{refusal: "restore refused: the database already exists; restore into a new Database (ADR 0016, section 3.7)"}, nil
		}
		source = src
		if d.GetStatus().GetRestoreState() == "" {
			exp.restoreState = database.RestorePending
		}
	}

	labels := ownedLabels(app)
	dataName, credName := name+dbDataSuffix, name+dbCredsSuffix
	dbName := database.DBName(name)

	// Storage is fixed once created: online resize is not in v1.
	vol, err := objects.Encode(&v1alpha1.VolumeSpec{
		Size: spec.GetSize(), AccessMode: resource.AccessRWO,
		Owner: &v1alpha1.VolumeOwner{Uid: dbUID, Gid: dbUID}, Mode: "0700", Retention: resource.RetentionRetain,
	})
	if err != nil {
		return expansion{}, err
	}
	if err := c.putOwned(ctx, v1alpha1.KindVolume, dataName, name, labels, vol, false); err != nil {
		return blockedBy(err)
	}

	// The credentials are generated once. An existing Secret is kept as it is.
	secret, err := c.st.Get(ctx, v1alpha1.KindSecret, credName)
	var creds []byte
	switch {
	case errors.Is(err, store.ErrNotFound):
		if creds, err = c.newDatabaseCredentials(ctx, name, dbName, source); err != nil {
			return expansion{}, err
		}
	case err != nil:
		return expansion{}, err
	default:
		creds = secret.Spec
	}
	if err := c.putOwned(ctx, v1alpha1.KindSecret, credName, name, labels, creds, false); err != nil {
		return blockedBy(err)
	}

	// A minor upgrade takes a verified backup first (section 3.8).
	if cur != "" && cur != version {
		refusal, hold, err := c.upgradeGate(ctx, name, cur, version)
		if err != nil {
			return expansion{}, err
		}
		if refusal != "" || hold != "" {
			return expansion{refusal: refusal, hold: hold, restoreState: exp.restoreState}, nil
		}
	}

	w := &v1alpha1.Workload{
		Metadata: &v1alpha1.ObjectMeta{Name: name},
		Spec: &v1alpha1.WorkloadSpec{
			Image:    image,
			Replicas: proto.Int32(1),
			Env: []*v1alpha1.EnvVar{
				secretEnv("POSTGRES_USER", credName, "username"),
				secretEnv("POSTGRES_PASSWORD", credName, "password"),
				secretEnv("POSTGRES_DB", credName, "database"),
				{Name: "PGDATA", Value: dbPGData},
			},
			Ports:     []*v1alpha1.Port{{ContainerPort: dbPort, Protocol: "tcp"}},
			Resources: &v1alpha1.Resources{Cpu: spec.GetResources().GetCpu(), Memory: spec.GetResources().GetMemory()},
			HealthCheck: &v1alpha1.HealthCheck{
				Exec:                &v1alpha1.ExecHealthCheck{Command: []string{"pg_isready", "-U", dbUser, "-d", dbName}},
				IntervalSeconds:     10,
				TimeoutSeconds:      5,
				FailureThreshold:    3,
				InitialDelaySeconds: 60,
			},
			Volumes: []*v1alpha1.VolumeMount{{Name: dataName, MountPath: dbDataPath}},
		},
	}
	resource.DefaultWorkload(w)
	if err := resource.ValidateWorkload(w); err != nil {
		return expansion{}, fmt.Errorf("database %s: expanded workload is invalid: %w", name, err)
	}
	wb, err := objects.Encode(w.Spec)
	if err != nil {
		return expansion{}, err
	}
	if err := c.putOwned(ctx, v1alpha1.KindWorkload, name, name, labels, wb, true); err != nil {
		return blockedBy(err)
	}

	svc, err := objects.Encode(&v1alpha1.ServiceSpec{
		Selector: name,
		Ports:    []*v1alpha1.ServicePort{{Port: dbPort, TargetPort: dbPort, Protocol: "tcp"}},
	})
	if err != nil {
		return expansion{}, err
	}
	if err := c.putOwned(ctx, v1alpha1.KindService, name, name, labels, svc, true); err != nil {
		return blockedBy(err)
	}
	return exp, nil
}

// blockedBy turns an ownership conflict into a refusal and passes any other
// error on.
func blockedBy(err error) (expansion, error) {
	if errors.Is(err, errNotOwned) {
		return expansion{refusal: err.Error()}, nil
	}
	return expansion{}, err
}

func secretEnv(name, secret, key string) *v1alpha1.EnvVar {
	return &v1alpha1.EnvVar{Name: name, ValueFrom: &v1alpha1.EnvSource{Secret: &v1alpha1.KeyRef{Name: secret, Key: key}}}
}

// newCredentials returns the encoded spec of a new credentials Secret: a random
// 256-bit password from crypto/rand, and the connection details.
func newCredentials(name, dbName string) ([]byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate database password: %w", err)
	}
	return encodeCredentials(name, dbName, base64.RawURLEncoding.EncodeToString(raw)) // URL-safe: needs no escaping in the URL
}

// encodeCredentials returns the encoded spec of the credentials Secret of a
// Database with password pw.
func encodeCredentials(name, dbName, pw string) ([]byte, error) {
	host := name + "." + v1alpha1.DNSDomain
	port := strconv.Itoa(dbPort)
	return objects.Encode(&v1alpha1.SecretSpec{Data: map[string]string{
		"username": dbUser,
		"password": pw,
		"database": dbName,
		"host":     host,
		"port":     port,
		"url":      fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable", dbUser, pw, host, port, dbName),
	}})
}

// ownedLabels is the label set of an owned object: the Database's app label,
// when it has one.
func ownedLabels(app string) map[string]string {
	if app == "" {
		return nil
	}
	return map[string]string{appLabel: app, resource.OwnedByLabel: "database"}
}

// putOwned makes kind/name exist and be claimed by the Database db. It creates
// the object (claim first, so a crash leaves a claim and never an unowned
// object) or, when mutable, updates a changed spec. It never adopts an object
// without this Database's claim.
func (c *Controller) putOwned(ctx context.Context, kind, name, db string, labels map[string]string, spec []byte, mutable bool) error {
	cur, err := c.st.Get(ctx, kind, name)
	if errors.Is(err, store.ErrNotFound) {
		if err := c.claimOwned(ctx, kind, name, db); err != nil {
			return err
		}
		if _, err := c.st.Create(ctx, store.Record{Kind: kind, Name: name, Spec: spec, Labels: labels}); err != nil {
			return fmt.Errorf("create %s %s: %w", kind, name, err)
		}
		return nil
	}
	if err != nil {
		return err
	}
	owner, err := c.ownerOf(ctx, kind, name)
	if err != nil {
		return err
	}
	if owner != db {
		return fmt.Errorf("%s %q already exists and is not owned by database %q: %w", kind, name, db, errNotOwned)
	}
	if !mutable || bytes.Equal(cur.Spec, spec) {
		return nil
	}
	if _, err := c.st.Update(ctx, kind, name, spec, cur.Generation); err != nil {
		return fmt.Errorf("update %s %s: %w", kind, name, err)
	}
	return nil
}

// claimOwned records that db owns kind/name. Creating the record is atomic, so
// of two racing claims one wins.
func (c *Controller) claimOwned(ctx context.Context, kind, name, db string) error {
	_, err := c.st.Create(ctx, store.Record{Kind: v1alpha1.KindOwnerClaim, Name: resource.OwnerKey(kind, name), Spec: []byte(db)})
	if err == nil {
		return nil
	}
	if !errors.Is(err, store.ErrExists) {
		return err
	}
	owner, err := c.ownerOf(ctx, kind, name)
	if err != nil {
		return err
	}
	if owner != db {
		return fmt.Errorf("%s %q is claimed by database %q: %w", kind, name, owner, errNotOwned)
	}
	return nil
}

// ownerOf returns the Database that owns kind/name, or "".
func (c *Controller) ownerOf(ctx context.Context, kind, name string) (string, error) {
	rec, err := c.st.Get(ctx, v1alpha1.KindOwnerClaim, resource.OwnerKey(kind, name))
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(rec.Spec), nil
}

// cascadeOwned deletes the objects whose owning Database is gone, and then
// their claims. A Volume waits while a Workload still mounts it.
func (c *Controller) cascadeOwned(ctx context.Context, s *state, dbs map[string]*v1alpha1.Database) []error {
	recs, err := c.st.List(ctx, v1alpha1.KindOwnerClaim)
	if err != nil {
		return []error{err}
	}
	var errs []error
	for _, r := range recs {
		kind, name, ok := strings.Cut(r.Name, "/")
		db := string(r.Spec)
		if !ok {
			continue
		}
		if _, alive := dbs[db]; alive {
			continue
		}
		gone, err := c.deleteOwned(ctx, s, kind, name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !gone {
			continue
		}
		if err := c.st.Delete(ctx, v1alpha1.KindOwnerClaim, r.Name, 0); err != nil && !errors.Is(err, store.ErrNotFound) {
			errs = append(errs, err)
			continue
		}
		c.log.Info("object of a deleted database removed", "kind", kind, "name", name, "database", db)
	}
	return errs
}

// deleteOwned removes one owned object. gone is false when it must wait.
func (c *Controller) deleteOwned(ctx context.Context, s *state, kind, name string) (bool, error) {
	switch kind {
	case v1alpha1.KindWorkload:
		rec, err := c.st.Get(ctx, kind, name)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return false, err
		default:
			w, err := objects.Workload(rec)
			if err != nil {
				return false, err
			}
			// Release the Volumes it mounts, as a user delete would.
			for _, m := range w.Spec.GetVolumes() {
				if err := c.releaseVolumeClaim(ctx, m.GetName(), name); err != nil {
					return false, err
				}
			}
		}
		if err := c.deleteIgnoringMissing(ctx, kind, name); err != nil {
			return false, err
		}
		delete(s.workloads, name)
	case v1alpha1.KindVolume:
		// A Volume a Workload still mounts is removed on a later pass, after
		// the Workload's claim is released. Its data is kept or removed by the
		// agent, as its retention says.
		if _, err := c.st.Get(ctx, v1alpha1.KindVolumeClaim, name); err == nil {
			return false, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return false, err
		}
		if err := c.deleteIgnoringMissing(ctx, kind, name); err != nil {
			return false, err
		}
		delete(s.volumes, name)
	case v1alpha1.KindService:
		if err := c.deleteIgnoringMissing(ctx, kind, name); err != nil {
			return false, err
		}
		delete(s.services, name)
	case v1alpha1.KindSecret:
		if err := c.deleteIgnoringMissing(ctx, kind, name); err != nil {
			return false, err
		}
	default:
		return false, fmt.Errorf("owner claim of unexpected kind %q", kind)
	}
	return true, nil
}

// releaseVolumeClaim drops the claim of volume if workload holds it.
func (c *Controller) releaseVolumeClaim(ctx context.Context, volume, workload string) error {
	rec, err := c.st.Get(ctx, v1alpha1.KindVolumeClaim, volume)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if string(rec.Spec) != workload {
		return nil
	}
	return c.deleteIgnoringMissing(ctx, v1alpha1.KindVolumeClaim, volume)
}

func (c *Controller) deleteIgnoringMissing(ctx context.Context, kind, name string) error {
	if err := c.st.Delete(ctx, kind, name, 0); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("delete %s %s: %w", kind, name, err)
	}
	return nil
}

// writeDatabaseStatus sets the phase of d. The phase comes from the expansion's
// refusal, or from the replicas of its Workload as of the last pass. A hold
// keeps the running version and shows the wait as the message.
func (c *Controller) writeDatabaseStatus(ctx context.Context, s *state, d *v1alpha1.Database, exp expansion) error {
	name := d.GetMetadata().GetName()
	prev := d.GetStatus().GetPhase()
	st := &v1alpha1.DatabaseStatus{
		Endpoint:     fmt.Sprintf("%s.%s:%d", name, v1alpha1.DNSDomain, dbPort),
		Version:      d.GetStatus().GetVersion(),
		RestoreState: d.GetStatus().GetRestoreState(),
	}
	if exp.restoreState != "" {
		st.RestoreState = exp.restoreState
	}
	// The version is the full major.minor the spec resolves to (16 is 16.15).
	version, _, _ := database.Resolve(d.GetSpec().GetEngine(), d.GetSpec().GetVersion())
	w, exists := s.workloads[name]
	switch {
	case exp.refusal != "":
		st.Phase, st.Message = "Failed", exp.refusal
	case exists && w.GetStatus().GetReadyReplicas() > 0:
		st.Phase = "Ready"
	case prev == "Ready" || prev == "Degraded":
		st.Phase, st.Message = "Degraded", "no replica is ready"
	default:
		st.Phase, st.Message = "Provisioning", "waiting for the database to start"
	}
	if exp.refusal == "" && exp.hold == "" {
		st.Version = version
	}
	if exp.hold != "" {
		st.Message = exp.hold
	}
	if st.Phase == "Ready" && exp.hold == "" && st.GetRestoreState() == database.RestoreFailed {
		st.Message = d.GetStatus().GetMessage() // keep the restore's error
	}
	if proto.Equal(d.GetStatus(), st) {
		return nil
	}
	b, err := objects.Encode(st)
	if err != nil {
		return err
	}
	if _, err := c.st.UpdateStatus(ctx, v1alpha1.KindDatabase, name, b); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("update status of database %s: %w", name, err)
	}
	return nil
}
