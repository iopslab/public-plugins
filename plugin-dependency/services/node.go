package services

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/crawlab-team/crawlab-core/controllers"
	"github.com/crawlab-team/go-trace"
	"github.com/crawlab-team/plugin-dependency/constants"
	"github.com/crawlab-team/plugin-dependency/entity"
	"github.com/crawlab-team/plugin-dependency/models"
	"github.com/gin-gonic/gin"
	"github.com/imroc/req"
	"go.mongodb.org/mongo-driver/bson"
	mongo2 "go.mongodb.org/mongo-driver/mongo"
	"net/url"
	"os/exec"
	"time"
)

type NodeService struct {
	*baseService
}

func (svc *NodeService) Init() {
	svc.api.GET("/node", svc.getList)
	svc.api.POST("/node/update", svc.update)
	svc.api.POST("/node/install", svc.install)
	svc.api.POST("/node/uninstall", svc.uninstall)
}

func (svc *NodeService) GetRepoList(c *gin.Context) {
	// query
	query := c.Query("query")
	pagination := controllers.MustGetPagination(c)

	// validate
	if query == "" {
		controllers.HandleErrorBadRequest(c, errors.New("empty query"))
		return
	}

	// request session
	reqSession := req.New()

	// set timeout
	reqSession.SetTimeout(15 * time.Second)

	// user agent
	ua := req.Header{"user-agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_14_6) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/94.0.4606.61 Safari/537.36"}

	// request url
	requestUrl := fmt.Sprintf("https://api.npms.io/v2/search?from=%d&q=%s&size=20", (pagination.Page-1)*pagination.Size, url.QueryEscape(query))

	// perform request
	res, err := reqSession.Get(requestUrl, ua)
	if err != nil {
		if res != nil {
			_, _ = c.Writer.Write(res.Bytes())
			_ = c.AbortWithError(res.Response().StatusCode, err)
			return
		}
		controllers.HandleErrorInternalServerError(c, err)
		return
	}

	// response
	var npmRes entity.NpmResponseList
	if err := res.ToJSON(&npmRes); err != nil {
		controllers.HandleErrorInternalServerError(c, err)
		return
	}

	// empty results
	if npmRes.Total == 0 {
		controllers.HandleSuccess(c)
		return
	}

	// dependencies
	var deps []models.Dependency
	var depNames []string
	for _, r := range npmRes.Results {
		d := models.Dependency{
			Name:          r.Package.Name,
			LatestVersion: r.Package.Version,
		}
		deps = append(deps, d)
		depNames = append(depNames, d.Name)
	}

	// total
	total := npmRes.Total

	// dependencies in db
	var depsResults []entity.DependencyResult
	pipelines := mongo2.Pipeline{
		{{
			"$match",
			bson.M{
				"type": constants.DependencyTypeNode,
				"name": bson.M{
					"$in": depNames,
				},
			},
		}},
		{{
			"$group",
			bson.M{
				"_id": "$name",
				"node_ids": bson.M{
					"$push": "$node_id",
				},
				"versions": bson.M{
					"$addToSet": "$version",
				},
			},
		}},
		{{
			"$project",
			bson.M{
				"name":     "$_id",
				"node_ids": "$node_ids",
				"versions": "$versions",
			},
		}},
	}
	if err := svc.parent.colD.Aggregate(pipelines, nil).All(&depsResults); err != nil {
		controllers.HandleErrorInternalServerError(c, err)
		return
	}

	// dependencies map
	depsResultsMap := map[string]entity.DependencyResult{}
	for _, dr := range depsResults {
		depsResultsMap[dr.Name] = dr
	}

	// iterate dependencies
	for i, d := range deps {
		dr, ok := depsResultsMap[d.Name]
		if ok {
			deps[i].Result = dr
		}
	}

	controllers.HandleSuccessWithListData(c, deps, total)
}

func (svc *NodeService) GetDependencies(params entity.UpdateParams) (deps []models.Dependency, err error) {
	cmd := exec.Command(params.Cmd, "list", "-g", "--json", "--depth", "0")
	data, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	var res entity.NpmListResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, err
	}
	for name, p := range res.Dependencies {
		d := models.Dependency{
			Name:    name,
			Version: p.Version,
		}
		d.Type = constants.DependencyTypeNode
		deps = append(deps, d)
	}
	return deps, nil
}

func (svc *NodeService) InstallDependencies(params entity.InstallParams) (err error) {
	// arguments
	var args []string

	// install
	args = append(args, "install")

	// global
	args = append(args, "-g")

	// proxy
	if params.Proxy != "" {
		args = append(args, "--registry")
		args = append(args, params.Proxy)
	}

	if params.UseConfig {
		// use config
	} else {
		// dependency names
		for _, depName := range params.Names {
			// upgrade
			if params.Upgrade {
				depName = depName + "@latest"
			}

			args = append(args, depName)
		}
	}

	// command
	cmd := exec.Command(params.Cmd, args...)

	// logging
	svc.parent._configureLogging(params.TaskId, cmd)

	// start
	if err := cmd.Start(); err != nil {
		return trace.TraceError(err)
	}

	// wait
	if err := cmd.Wait(); err != nil {
		return trace.TraceError(err)
	}

	return nil
}

func (svc *NodeService) UninstallDependencies(params entity.UninstallParams) (err error) {
	// arguments
	var args []string

	// uninstall
	args = append(args, "uninstall")
	args = append(args, "-g")

	// dependency names
	for _, depName := range params.Names {
		args = append(args, depName)
	}

	// command
	cmd := exec.Command(params.Cmd, args...)

	// logging
	svc.parent._configureLogging(params.TaskId, cmd)

	// start
	if err := cmd.Start(); err != nil {
		return trace.TraceError(err)
	}

	// wait
	if err := cmd.Wait(); err != nil {
		return trace.TraceError(err)
	}

	return nil
}

func (svc *NodeService) GetLatestVersion(dep models.Dependency) (v string, err error) {
	// not exists in cache, request from pypi
	reqSession := req.New()

	// set timeout
	reqSession.SetTimeout(60 * time.Second)

	// user agent
	ua := req.Header{"user-agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_14_6) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/94.0.4606.61 Safari/537.36"}

	// request url
	requestUrl := fmt.Sprintf("https://api.npms.io/v2/package/%s", dep.Name)

	// perform request
	res, err := reqSession.Get(requestUrl, ua)
	if err != nil {
		return "", trace.TraceError(err)
	}

	// response
	var npmRes entity.NpmResponseDetail
	if err := res.ToJSON(&npmRes); err != nil {
		return "", trace.TraceError(err)
	}

	// version
	v = npmRes.Collected.Metadata.Version

	return v, nil
}

func NewNodeService(parent *Service) (svc *NodeService) {
	svc = &NodeService{}
	baseSvc := newBaseService(
		svc,
		parent,
		constants.DependencyTypeNode,
		entity.MessageCodes{
			Update:    constants.MessageCodeNodeUpdate,
			Save:      constants.MessageCodeNodeSave,
			Install:   constants.MessageCodeNodeInstall,
			Uninstall: constants.MessageCodeNodeUninstall,
		},
	)
	svc.baseService = baseSvc
	return svc
}
