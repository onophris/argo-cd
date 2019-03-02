package repository

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"reflect"

	"github.com/ghodss/yaml"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	appsv1 "github.com/argoproj/argo-cd/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/reposerver"
	"github.com/argoproj/argo-cd/reposerver/repository"
	"github.com/argoproj/argo-cd/server/rbacpolicy"
	"github.com/argoproj/argo-cd/util"
	"github.com/argoproj/argo-cd/util/cache"
	"github.com/argoproj/argo-cd/util/db"
	"github.com/argoproj/argo-cd/util/git"
	"github.com/argoproj/argo-cd/util/kustomize"
	"github.com/argoproj/argo-cd/util/rbac"
)

// Server provides a Repository service
type Server struct {
	db            db.ArgoDB
	repoClientset reposerver.Clientset
	enf           *rbac.Enforcer
	cache         *cache.Cache
}

// NewServer returns a new instance of the Repository service
func NewServer(
	repoClientset reposerver.Clientset,
	db db.ArgoDB,
	enf *rbac.Enforcer,
	cache *cache.Cache,
) *Server {
	return &Server{
		db:            db,
		repoClientset: repoClientset,
		enf:           enf,
		cache:         cache,
	}
}

func (s *Server) HydrateConnectionState(ctx context.Context, repo *appsv1.Repository) {
	connectionState, err := s.cache.GetRepoConnectionState(repo.Repo)
	if err == nil {
		repo.ConnectionState = connectionState
		return
	}
	now := metav1.Now()

	err = git.TestRepo(repo.Repo, string(repo.Type), repo.Username, repo.Password, repo.SSHPrivateKey)

	if err != nil {
		repo.ConnectionState = appsv1.ConnectionState{
			Status:     appsv1.ConnectionStatusFailed,
			Message:    fmt.Sprintf("Unable to connect to repository: %v", err),
			ModifiedAt: &now,
		}
		return
	}
	err = s.cache.SetRepoConnectionState(repo.Repo, &connectionState)
	if err != nil {
		log.Warnf("cache set error %s: %v", repo.Repo, err)
	}

	repo.ConnectionState = appsv1.ConnectionState{
		Status:     appsv1.ConnectionStatusSuccessful,
		ModifiedAt: &now,
	}
}

// List returns list of repositories
func (s *Server) List(ctx context.Context, q *RepoQuery) (*appsv1.RepositoryList, error) {
	repos, err := s.db.ListRepositories(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]appsv1.Repository, 0)
	if repos != nil {
		for _, repo := range repos {
			if s.enf.Enforce(ctx.Value("claims"), rbacpolicy.ResourceRepositories, rbacpolicy.ActionGet, repo) {
				items = append(items, appsv1.Repository{Name: repo.Name, Repo: repo.Repo, Type: repo.Type})
			}
		}
	}
	err = util.RunAllAsync(len(items), func(i int) error {
		s.HydrateConnectionState(ctx, &items[i])
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &appsv1.RepositoryList{Items: items}, nil
}

func (s *Server) listAppsPaths(
	ctx context.Context, repoClient repository.RepositoryServiceClient, repo *appsv1.Repository, revision string, subPath string) (map[string]appsv1.ApplicationSourceType, error) {

	if revision == "" {
		revision = "HEAD"
	}

	ksonnetRes, err := repoClient.ListDir(ctx, &repository.ListDirRequest{Repo: repo, Revision: revision, Path: path.Join(subPath, "*app.yaml")})
	if err != nil {
		return nil, err
	}
	componentRes, err := repoClient.ListDir(ctx, &repository.ListDirRequest{Repo: repo, Revision: revision, Path: path.Join(subPath, "*components/params.libsonnet")})
	if err != nil {
		return nil, err
	}

	helmRes, err := repoClient.ListDir(ctx, &repository.ListDirRequest{Repo: repo, Revision: revision, Path: path.Join(subPath, "*Chart.yaml")})
	if err != nil {
		return nil, err
	}

	kustomizationRes, err := getKustomizationRes(ctx, repoClient, repo, revision, subPath)
	if err != nil {
		return nil, err
	}

	componentDirs := make(map[string]interface{})
	for _, i := range componentRes.Items {
		d := filepath.Dir(filepath.Dir(i))
		componentDirs[d] = struct{}{}
	}

	pathToType := make(map[string]appsv1.ApplicationSourceType)
	for _, i := range ksonnetRes.Items {
		d := filepath.Dir(i)
		if _, ok := componentDirs[d]; ok {
			pathToType[i] = appsv1.ApplicationSourceTypeKsonnet
		}
	}

	for i := range helmRes.Items {
		pathToType[helmRes.Items[i]] = appsv1.ApplicationSourceTypeHelm
	}

	for i := range kustomizationRes.Items {
		pathToType[kustomizationRes.Items[i]] = appsv1.ApplicationSourceTypeKustomize
	}
	return pathToType, nil
}

func getKustomizationRes(ctx context.Context, repoClient repository.RepositoryServiceClient, repo *appsv1.Repository, revision string, subPath string) (*repository.FileList, error) {
	for _, kustomization := range kustomize.KustomizationNames {
		request := repository.ListDirRequest{Repo: repo, Revision: revision, Path: path.Join(subPath, "*"+kustomization)}
		kustomizationRes, err := repoClient.ListDir(ctx, &request)
		if err != nil {
			return nil, err
		}
		return kustomizationRes, nil
	}
	return nil, errors.New("could not find kustomization")
}

// ListKsonnetApps returns list of Ksonnet apps in the repo
func (s *Server) ListApps(ctx context.Context, q *RepoAppsQuery) (*RepoAppsResponse, error) {
	if err := s.enf.EnforceErr(ctx.Value("claims"), rbacpolicy.ResourceRepositories, rbacpolicy.ActionGet, q.Repo); err != nil {
		return nil, err
	}
	repo, err := s.db.GetRepository(ctx, q.Repo)
	if err != nil {
		if errStatus, ok := status.FromError(err); ok && errStatus.Code() == codes.NotFound {
			repo = &appsv1.Repository{
				Repo: q.Repo,
			}
		} else {
			return nil, err
		}
	}

	// Test the repo
	conn, repoClient, err := s.repoClientset.NewRepositoryClient()
	if err != nil {
		return nil, err
	}
	defer util.Close(conn)

	revision := q.Revision
	if revision == "" {
		revision = "HEAD"
	}

	paths, err := s.listAppsPaths(ctx, repoClient, repo, revision, "")
	if err != nil {
		return nil, err
	}
	items := make([]*AppInfo, 0)
	for appFilePath, appType := range paths {
		items = append(items, &AppInfo{Path: path.Dir(appFilePath), Type: string(appType)})
	}
	return &RepoAppsResponse{Items: items}, nil
}

func (s *Server) GetAppDetails(ctx context.Context, q *RepoAppDetailsQuery) (*RepoAppDetailsResponse, error) {
	if err := s.enf.EnforceErr(ctx.Value("claims"), rbacpolicy.ResourceRepositories, rbacpolicy.ActionGet, q.Repo); err != nil {
		return nil, err
	}
	repo, err := s.db.GetRepository(ctx, q.Repo)
	if err != nil {
		if errStatus, ok := status.FromError(err); ok && errStatus.Code() == codes.NotFound {
			repo = &appsv1.Repository{
				Repo: q.Repo,
			}
		} else {
			return nil, err
		}
	}

	// Test the repo
	conn, repoClient, err := s.repoClientset.NewRepositoryClient()
	if err != nil {
		return nil, err
	}
	defer util.Close(conn)

	revision := q.Revision
	if revision == "" {
		revision = "HEAD"
	}

	paths, err := s.listAppsPaths(ctx, repoClient, repo, revision, q.Path)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return &RepoAppDetailsResponse{
			Type:      string(appsv1.ApplicationSourceTypeDirectory),
			Directory: &DirectoryAppSpec{},
		}, nil
	}

	var appPath string
	var appSourceType appsv1.ApplicationSourceType
	for appPath, appSourceType = range paths {
		break
	}

	appSpecRes, err := repoClient.GetFile(ctx, &repository.GetFileRequest{
		Repo:     repo,
		Revision: revision,
		Path:     appPath,
	})
	if err != nil {
		return nil, err
	}

	switch appSourceType {
	case appsv1.ApplicationSourceTypeKsonnet:
		var appSpec KsonnetAppSpec
		appSpec.Path = q.Path
		err = yaml.Unmarshal(appSpecRes.Data, &appSpec)
		if err != nil {
			return nil, err
		}
		return &RepoAppDetailsResponse{
			Type:    string(appSourceType),
			Ksonnet: &appSpec,
		}, nil
	case appsv1.ApplicationSourceTypeHelm:
		var appSpec HelmAppSpec
		appSpec.Path = q.Path
		err = yaml.Unmarshal(appSpecRes.Data, &appSpec)
		if err != nil {
			return nil, err
		}
		valuesFilesRes, err := repoClient.ListDir(ctx, &repository.ListDirRequest{
			Revision: revision,
			Repo:     repo,
			Path:     path.Join(q.Path, "*values*.yaml"),
		})
		if err != nil {
			return nil, err
		}
		appSpec.ValueFiles = make([]string, len(valuesFilesRes.Items))
		for i := range valuesFilesRes.Items {
			valueFilePath, err := filepath.Rel(q.Path, valuesFilesRes.Items[i])
			if err != nil {
				return nil, err
			}
			appSpec.ValueFiles[i] = valueFilePath
		}
		return &RepoAppDetailsResponse{
			Type: string(appSourceType),
			Helm: &appSpec,
		}, nil
	case appsv1.ApplicationSourceTypeKustomize:
		appSpec := KustomizeAppSpec{
			Path: q.Path,
		}
		return &RepoAppDetailsResponse{
			Type:      string(appSourceType),
			Kustomize: &appSpec,
		}, nil
	}
	return nil, status.Errorf(codes.InvalidArgument, "specified application path is not supported")
}

// Create creates a repository
func (s *Server) Create(ctx context.Context, q *RepoCreateRequest) (*appsv1.Repository, error) {
	if err := s.enf.EnforceErr(ctx.Value("claims"), rbacpolicy.ResourceRepositories, rbacpolicy.ActionCreate, q.Repo.Repo); err != nil {
		return nil, err
	}
	r := q.Repo
	err := git.TestRepo(r.Repo, string(r.Type), r.Username, r.Password, r.SSHPrivateKey)
	if err != nil {
		return nil, err
	}

	r.ConnectionState = appsv1.ConnectionState{Status: appsv1.ConnectionStatusSuccessful}
	repo, err := s.db.CreateRepository(ctx, r)
	if status.Convert(err).Code() == codes.AlreadyExists {
		// act idempotent if existing spec matches new spec
		existing, getErr := s.db.GetRepository(ctx, r.Repo)
		if getErr != nil {
			return nil, status.Errorf(codes.Internal, "unable to check existing repository details: %v", getErr)
		}

		// repository ConnectionState may differ, so make consistent before testing
		existing.ConnectionState = r.ConnectionState
		if reflect.DeepEqual(existing, r) {
			repo, err = existing, nil
		} else if q.Upsert {
			return s.Update(ctx, &RepoUpdateRequest{Repo: r})
		} else {
			return nil, status.Errorf(codes.InvalidArgument, "existing repository spec is different; use upsert flag to force update")
		}
	}
	return &appsv1.Repository{Repo: repo.Repo, Type: repo.Type}, err
}

// Update updates a repository
func (s *Server) Update(ctx context.Context, q *RepoUpdateRequest) (*appsv1.Repository, error) {
	// TODO - allow update of type?
	if err := s.enf.EnforceErr(ctx.Value("claims"), rbacpolicy.ResourceRepositories, rbacpolicy.ActionUpdate, q.Repo.Repo); err != nil {
		return nil, err
	}
	_, err := s.db.UpdateRepository(ctx, q.Repo)
	return &appsv1.Repository{Repo: q.Repo.Repo}, err
}

// Delete updates a repository
func (s *Server) Delete(ctx context.Context, q *RepoQuery) (*RepoResponse, error) {
	if err := s.enf.EnforceErr(ctx.Value("claims"), rbacpolicy.ResourceRepositories, rbacpolicy.ActionDelete, q.Repo); err != nil {
		return nil, err
	}

	// invalidate cache
	if err := s.cache.SetRepoConnectionState(q.Repo, nil); err == nil {
		log.Errorf("error invalidating cache: %v", err)
	}

	err := s.db.DeleteRepository(ctx, q.Repo)
	return &RepoResponse{}, err
}
