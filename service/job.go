package service

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/ppkg/distributed-scheduler/core"
	"github.com/ppkg/distributed-scheduler/dto"
	"github.com/ppkg/distributed-scheduler/errCode"
	"github.com/ppkg/distributed-scheduler/model"
	"github.com/ppkg/distributed-scheduler/proto/job"
	"github.com/ppkg/distributed-scheduler/repository"
	"github.com/ppkg/distributed-scheduler/repository/impl"

	"github.com/golang/protobuf/ptypes/empty"
	"github.com/ppkg/glog"
	"gorm.io/gorm"
)

type jobService struct {
	appCtx   *core.ApplicationContext
	jobRepo  repository.JobRepository
	taskRepo repository.TaskRepository
}

// 异步提交job
func (s *jobService) AsyncSubmit(stream job.JobService_AsyncSubmitServer) error {
	jobInfo, err := s.receiveAsyncJobStream(stream)
	if err != nil {
		glog.Errorf("jobService/AsyncSubmit 接收job数据异常,err:%+v", err)
		return err
	}

	err = s.persistence(jobInfo)
	if err != nil {
		glog.Errorf("jobService/AsyncSubmit 持久化job数据异常,err:%+v", err)
		return err
	}

	// 重新加载最新job信息
	jobInfo, err = s.reloadJobInfo(jobInfo)
	if err != nil {
		glog.Errorf("jobService/AsyncSubmit 重新加载最新job信息异常,err:%+v", err)
		return err
	}

	go func() {
		_ = s.appCtx.StartJob(jobInfo)
	}()

	return stream.SendAndClose(
		&job.AsyncSubmitResponse{
			Id: jobInfo.Job.Id,
		},
	)
}

// 异步通知
func (s *jobService) AsyncNotify(ctx context.Context, req *job.AsyncNotifyRequest) (*empty.Empty, error) {
	panic("not implemented")
}

// 同步提交job
func (s *jobService) SyncSubmit(stream job.JobService_SyncSubmitServer) error {
	jobInfo, err := s.receiveSyncJobStream(stream)
	if err != nil {
		glog.Errorf("jobService/SyncSubmit 接收job数据异常,err:%+v", err)
		return err
	}
	jobInfo.Job.IsAsync = 0

	err = s.persistence(jobInfo)
	if err != nil {
		glog.Errorf("jobService/SyncSubmit 持久化job数据异常,err:%+v", err)
		return err
	}

	// 重新加载最新job信息
	jobInfo, err = s.reloadJobInfo(jobInfo)
	if err != nil {
		glog.Errorf("jobService/SyncSubmit 重新加载最新job信息异常,err:%+v", err)
		return err
	}

	err = s.appCtx.StartJob(jobInfo)
	if err != nil {
		glog.Errorf("jobService/SyncSubmit 执行job运算异常,id:%d,err:%+v", jobInfo.Job.Id, err)
		return err
	}

	return stream.SendAndClose(&job.SyncSubmitResponse{
		Id:     jobInfo.Job.Id,
		Status: jobInfo.Job.Status,
		Result: jobInfo.Job.Result,
	})
}

// 重新加载job信息
func (s *jobService) reloadJobInfo(jobInfo *dto.JobInfo) (*dto.JobInfo, error) {
	if jobInfo == nil {
		jobInfo = &dto.JobInfo{}
	}
	var err error
	jobInfo.Job, err = s.jobRepo.FindById(s.appCtx.Db, jobInfo.Job.Id)
	if err != nil {
		return jobInfo, err
	}
	if jobInfo.Job == nil {
		return jobInfo, errCode.ToGrpcErr(errCode.ErrJobNotFound)
	}
	jobInfo.TaskList, err = s.taskRepo.List(s.appCtx.Db, map[string]interface{}{
		"jobId": jobInfo.Job.Id,
	})
	return jobInfo, err
}

// 持久化job信息
func (s *jobService) persistence(jobInfo *dto.JobInfo) error {
	return s.appCtx.Db.Transaction(func(tx *gorm.DB) error {
		err := s.jobRepo.Save(tx, jobInfo.Job)
		if err != nil {
			return err
		}
		for _, item := range jobInfo.TaskList {
			item.JobId = jobInfo.Job.Id
		}
		return s.taskRepo.BatchSave(tx, jobInfo.TaskList)
	})
}

// 接收同步job流
func (s *jobService) receiveSyncJobStream(stream job.JobService_SyncSubmitServer) (*dto.JobInfo, error) {
	jobInfo := &dto.JobInfo{}
	var firstPlugin string
	var sharding int32 = 0
	for {
		r, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return jobInfo, err
		}
		if len(r.PluginSet) == 0 {
			return jobInfo, errCode.ToGrpcErr(errCode.ErrPluginSetEmpty)
		}
		// 初始化job数据
		if jobInfo.Job == nil {
			jobInfo.Job = &model.Job{
				Name:                   r.Name,
				PluginSet:              strings.Join(r.PluginSet, ","),
				Label:                  r.Label,
				Source:                 r.Source,
				TaskExceptionOperation: r.TaskExceptionOperation,
			}
			firstPlugin = r.PluginSet[0]
		}
		jobInfo.AppendSafeTask(&model.Task{
			Sharding: sharding,
			Name:     fmt.Sprintf("%s-%d", jobInfo.Job.Name, sharding),
			Input:    r.Data,
			Plugin:   firstPlugin,
		})
		sharding++
	}

	jobInfo.Job.Size = int32(len(jobInfo.TaskList))

	return jobInfo, nil
}

// 接收异步job流
func (s *jobService) receiveAsyncJobStream(stream job.JobService_AsyncSubmitServer) (*dto.JobInfo, error) {
	jobInfo := &dto.JobInfo{}
	var firstPlugin string
	var sharding int32 = 0
	for {
		r, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return jobInfo, err
		}
		if len(r.PluginSet) == 0 {
			return jobInfo, errCode.ToGrpcErr(errCode.ErrPluginSetEmpty)
		}
		// 初始化job数据
		if jobInfo.Job == nil {
			jobInfo.Job = &model.Job{
				Name:                   r.Name,
				Type:                   r.Type,
				PluginSet:              strings.Join(r.PluginSet, ","),
				IsAsync:                1,
				Label:                  r.Label,
				Source:                 r.Source,
				TaskExceptionOperation: r.TaskExceptionOperation,
			}
			if r.IsNotify {
				jobInfo.Job.IsNotify = 1
			}
			firstPlugin = r.PluginSet[0]
		}
		jobInfo.AppendSafeTask(&model.Task{
			Sharding: sharding,
			Name:     fmt.Sprintf("%s-%d", jobInfo.Job.Name, sharding),
			Input:    r.Data,
			Plugin:   firstPlugin,
		})
		sharding++
	}

	jobInfo.Job.Size = int32(len(jobInfo.TaskList))

	return jobInfo, nil
}

func NewJobService(ctx *core.ApplicationContext) job.JobServiceServer {
	return &jobService{
		appCtx:   ctx,
		jobRepo:  impl.NewJobRepository(),
		taskRepo: impl.NewTaskRepository(),
	}
}
