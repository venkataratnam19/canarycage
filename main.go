package main

import (
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/aws/aws-sdk-go/service/cloudwatch"
	"regexp"
	"github.com/pkg/errors"
	"fmt"
	"time"
	"golang.org/x/sync/errgroup"
	"math"
	"github.com/apex/log"
	"github.com/aws/aws-sdk-go/service/ecs/ecsiface"
	"github.com/aws/aws-sdk-go/service/cloudwatch/cloudwatchiface"
)

func main() {
	envars, err := EnsureEnvars()
	ses, err := session.NewSession(&aws.Config{
		Region: &envars.Region,
	})
	if err != nil {
		log.Fatalf("failed to create new AWS session due to: %s", err.Error())
		panic(err)
	}
	awsEcs := ecs.New(ses)
	cw := cloudwatch.New(ses)
	if err := envars.StartGradualRollOut(awsEcs, cw); err != nil {
		log.Fatalf("😭failed roll out new tasks due to: %s", err.Error())
		panic(err)
	}
	log.Infof("🎉service roll out has completed successfully!🎉")
}


func (envars *Envars) CreateNextTaskDefinition(awsEcs ecsiface.ECSAPI) (*ecs.TaskDefinition, error) {
	taskDefinition, err := UnmarshalTaskDefinition(envars.NextTaskDefinitionBase64)
	if err != nil {
		return nil, err
	}
	if out, err := awsEcs.RegisterTaskDefinition(taskDefinition); err != nil {
		return nil, err
	} else {
		return out.TaskDefinition, nil
	}
}

func (envars *Envars) CreateNextService(awsEcs ecsiface.ECSAPI, nextTaskDefinitionArn *string) (*ecs.Service, error) {
	serviceDefinition, err := UnmarshalServiceDefinition(envars.NextServiceDefinitionBase64)
	if err != nil {
		return nil, err
	}
	if out, err := awsEcs.CreateService(serviceDefinition); err != nil {
		return nil, err
	} else {
		return out.Service, nil
	}
}

func ExtractAlbId(arn string) (string, error) {
	regex := regexp.MustCompile(`^.+(app/.+?)$`)
	if group := regex.FindStringSubmatch(arn); group == nil || len(group) == 1 {
		return "", errors.New(fmt.Sprintf("could not find alb id in '%s'", arn))
	} else {
		return group[1], nil
	}
}

func ExtractTargetGroupId(arn string) (string, error) {
	regex := regexp.MustCompile(`^.+(targetgroup/.+?)$`)
	if group := regex.FindStringSubmatch(arn); group == nil || len(group) == 1 {
		return "", errors.New(fmt.Sprintf("could not find target group id in '%s'", arn))
	} else {
		return group[1], nil
	}
}

type ServiceHealth struct {
	availability float64
	responseTime float64
}

func (envars *Envars) AccumulatePeriodicServiceHealth(
	cw cloudwatchiface.CloudWatchAPI,
	targetGroupArn string,
	epoch time.Time,
) (*ServiceHealth, error) {
	lbArn := envars.LoadBalancerArn
	tgArn := targetGroupArn
	lbKey := "LoadBalancer"
	lbId, _ := ExtractAlbId(lbArn)
	tgKey := "TargetGroup"
	tgId, _ := ExtractTargetGroupId(tgArn)
	nameSpace := "ApplicationELB"
	period := int64(envars.RollOutPeriod.Seconds())
	dimensions := []*cloudwatch.Dimension{
		{
			Name:  &lbKey,
			Value: &lbId,
		}, {
			Name:  &tgKey,
			Value: &tgId,
		},
	}
	endTime := epoch
	endTime.Add(envars.RollOutPeriod)
	// ロールアウトの検証期間だけ待つ
	timer := time.NewTimer(envars.RollOutPeriod)
	<-timer.C
	getStatics := func(metricName string, unit string) (float64, error) {
		out, err := cw.GetMetricStatistics(&cloudwatch.GetMetricStatisticsInput{
			Namespace:  &nameSpace,
			Dimensions: dimensions,
			MetricName: &metricName,
			StartTime:  &epoch,
			EndTime:    &endTime,
			Period:     &period,
			Unit:       &unit,
		})
		if err != nil {
			log.Fatalf("failed to get CloudWatch's '%s' metric statistics due to: %s", metricName, err.Error())
			return 0, err
		}
		var ret float64 = 0
		switch unit {
		case "Sum":
			for _, v := range out.Datapoints {
				ret += *v.Sum
			}
		case "Average":
			for _, v := range out.Datapoints {
				ret += *v.Average
			}
			if l := len(out.Datapoints); l > 0 {
				ret /= float64(l)
			} else {
				err = errors.New("no data points found")
			}
		default:
			err = errors.New(fmt.Sprintf("unsuported unit type: %s", unit))
		}
		return ret, err

	}
	eg := errgroup.Group{}
	requestCnt := 0.0
	elb5xxCnt := 0.0
	target5xxCnt := 0.0
	responseTime := 0.0
	accumulate := func(metricName string, unit string, dest *float64) func() (error) {
		return func() (error) {
			if v, err := getStatics(metricName, unit); err != nil {
				log.Errorf("failed to accumulate CloudWatch's '%s' metric value due to: %s", metricName, err.Error())
				return err
			} else {
				*dest = v
				return nil
			}
		}
	}
	eg.Go(accumulate("RequestCount", "Sum", &requestCnt))
	eg.Go(accumulate("HTTPCode_ELB_5XX_Count", "Sum", &elb5xxCnt))
	eg.Go(accumulate("HTTPCode_Target_5XX_Count", "Sum", &target5xxCnt))
	eg.Go(accumulate("TargetResponseTime", "Average", &responseTime))
	if err := eg.Wait(); err != nil {
		log.Errorf("failed to accumulate periodic service health due to: %s", err.Error())
		return nil, err
	} else {
		if requestCnt == 0 && elb5xxCnt == 0 {
			return nil, errors.New("failed to get precise metric data")
		} else {
			avl := (requestCnt - target5xxCnt) / (requestCnt + elb5xxCnt)
			avl = math.Max(0, math.Min(1, avl))
			return &ServiceHealth{
				availability: avl,
				responseTime: responseTime,
			}, nil
		}
	}
}

func (envars *Envars) StartGradualRollOut(awsEcs ecsiface.ECSAPI, cw cloudwatchiface.CloudWatchAPI) (error) {
	// task-definition-nextを作成する
	taskDefinition, err := envars.CreateNextTaskDefinition(awsEcs)
	if err != nil {
		log.Fatalf("😭failed to create new task definition due to: %s", err.Error())
		return err
	}
	// service-nextを作成する
	nextService, err := envars.CreateNextService(awsEcs, taskDefinition.TaskDefinitionArn)
	if err != nil {
		log.Fatalf("😭failed to create new service due to: %s", err.Error())
		return err
	}
	services := []*string{ nextService.ServiceName }
	if err := awsEcs.WaitUntilServicesStable(&ecs.DescribeServicesInput{
		Cluster:  &envars.Cluster,
		Services: services,
	}); err != nil {
		log.Fatalf("created next service state hasn't reached STABLE state within an interval due to: %s", err.Error())
		return err
	}
	// ロールバックのためのデプロイを始める前の現在のサービスのタスク数
	var originalRunningTaskCount int
	if out, err := awsEcs.DescribeServices(&ecs.DescribeServicesInput{
		Cluster: &envars.Cluster,
		Services: []*string{
			&envars.CurrentServiceArn,
		},
	}); err != nil {
		log.Errorf("failed to describe current service due to: %s", err.Error())
		return err
	} else {
		originalRunningTaskCount = int(*out.Services[0].RunningCount)
	}
	// ロールアウトで置き換えられたタスクの数
	replacedCnt := 0
	// ロールアウト実行回数
	rollOutCnt := 0
	// 推定ロールアウト施行回数
	estimatedRollOutCount := func() int {
		ret := 0
		for i := 0.0; ; i += 1.0 {
			add := int(math.Pow(2, i))
			if ret+add > originalRunningTaskCount {
				break
			}
			ret += add
		}
		return ret
	}()
	lb := nextService.LoadBalancers[0]
	// next serviceのperiodic healthが安定し、current serviceのtaskの数が0になるまで繰り返す
	for {
		if estimatedRollOutCount < rollOutCnt {
			return errors.New(
				fmt.Sprintf(
					"estimated roll out attempts count exceeded: estimated=%d, replaced=%d/%d",
					estimatedRollOutCount, replacedCnt, originalRunningTaskCount,
				),
			)
		}
		epoch := time.Now()
		health, err := envars.AccumulatePeriodicServiceHealth(cw, *lb.TargetGroupArn, epoch)
		if err != nil {
			return err
		}
		out, err := awsEcs.DescribeServices(&ecs.DescribeServicesInput{
			Cluster: &envars.Cluster,
			Services: []*string{
				&envars.CurrentServiceArn,
				nextService.ServiceArn,
			},
		})
		if err != nil {
			log.Errorf("failed to describe next service due to: %s", err.Error())
			return err
		}
		currentService := out.Services[0]
		nextService := out.Services[1]
		if *currentService.RunningCount == 0 && int(*nextService.RunningCount) >= originalRunningTaskCount {
			// すべてのタスクが完全に置き換わったら、current serviceを消す
			if _, err := awsEcs.DeleteService(&ecs.DeleteServiceInput{
				Cluster: &envars.Cluster,
				Service: currentService.ServiceName,
			}); err != nil {
				log.Fatalf("failed to delete empty current service due to: %s", err.Error())
				return err
			}
			if err := awsEcs.WaitUntilServicesInactive(&ecs.DescribeServicesInput{
				Cluster:  &envars.Cluster,
				Services: []*string{currentService.ServiceArn},
			}); err != nil {
				log.Errorf("deleted current service state hasn't reached INACTIVE state within an interval due to: %s", err.Error())
				return err
			}
			log.Infof("all current tasks have been replaced into next tasks")
			return nil
		}
		if health.availability <= envars.AvailabilityThreshold && envars.ResponseTimeThreshold <= health.responseTime {
			// カナリアテストに合格した場合、次のロールアウトに入る
			if err := envars.RollOut(awsEcs, currentService, nextService, &replacedCnt, &rollOutCnt); err != nil {
				log.Fatalf("failed to roll out tasks due to: %s", err.Error())
				return err
			}
			log.Infof(
				"😙 %dth canary test has passed. %d/%d tasks roll outed: availability=%f (thresh: %f), responseTime=%f (thresh: %f)",
				rollOutCnt, replacedCnt, originalRunningTaskCount,
				health.availability, envars.AvailabilityThreshold, health.responseTime, envars.ResponseTimeThreshold,
			)
		} else {
			// カナリアテストに失敗した場合、task-definition-currentでデプロイを始めた段階のcurrent serviceのタスク数まで戻す
			log.Warnf(
				"😢 %dth canary test haven't passed: availability=%f (thresh: %f), responseTime=%f (thresh: %f)",
				rollOutCnt, health.availability, envars.AvailabilityThreshold, health.responseTime, envars.ResponseTimeThreshold,
			)
			return envars.Rollback(awsEcs, currentService, *nextService.ServiceName, originalRunningTaskCount)
		}
	}
}

func (envars *Envars) RollOut(
	awsEcs ecsiface.ECSAPI,
	currentService *ecs.Service,
	nextService *ecs.Service,
	replacedCount *int,
	rollOutCount *int,
) error {
	launchType := "FARGATE"
	desiredStatus := "RUNNING"
	if out, err := awsEcs.ListTasks(&ecs.ListTasksInput{
		Cluster:       &envars.Cluster,
		ServiceName:   currentService.ServiceName,
		DesiredStatus: &desiredStatus,
		LaunchType:    &launchType,
	}); err != nil {
		log.Errorf("failed to list current tasks due to: %s", err.Error())
		return err
	} else {
		tasks := out.TaskArns
		//TODO: 2018/08/01 ここでRUNNINGタスクの中から止めるものを選択するロジックを考えるべきかもしれない by sakurai
		numToBeReplaced := int(math.Pow(2, float64(*rollOutCount)))
		eg := errgroup.Group{}
		for i := 0; i < numToBeReplaced && len(tasks) > 0; i++ {
			task := tasks[0]
			tasks = tasks[1:]
			// current-serviceから1つタスクを止めて、next-serviceに1つタスクを追加する
			eg.Go(func() error {
				subEg := errgroup.Group{}
				subEg.Go(func() error {
					out, err := awsEcs.StopTask(&ecs.StopTaskInput{
						Cluster: &envars.Cluster,
						Task:    task,
					})
					if err != nil {
						log.Errorf("failed to stop task from current service: taskArn=%s", *task)
						return err
					}
					if err := awsEcs.WaitUntilTasksStopped(&ecs.DescribeTasksInput{
						Cluster: &envars.Cluster,
						Tasks:   []*string{out.Task.TaskArn},
					}); err != nil {
						log.Errorf("stopped current task state hasn't reached STOPPED state within maximum attempt windows: taskArn=%s", out.Task.TaskArn)
						return err
					}
					return nil
				})
				subEg.Go(func() error {
					group := fmt.Sprintf("service:%s", *nextService.ServiceName)
					out, err := awsEcs.StartTask(&ecs.StartTaskInput{
						Cluster:        &envars.Cluster,
						TaskDefinition: nextService.TaskDefinition,
						Group:          &group,
					})
					if err != nil {
						log.Errorf("failed to start task into next service: taskArn=%s", out.Tasks[0].TaskArn)
						return err
					}
					if err := awsEcs.WaitUntilTasksRunning(&ecs.DescribeTasksInput{
						Cluster: &envars.Cluster,
						Tasks:   []*string{out.Tasks[0].TaskArn},
					}); err != nil {
						log.Errorf("launched next task state hasn't reached RUNNING state within maximum attempt windows: taskArn=%s", out.Tasks[0].TaskArn)
						return err
					}
					return nil
				})
				if err := subEg.Wait(); err != nil {
					log.Fatalf("failed to replace tasks due to: %s", err.Error())
					return err
				}
				*replacedCount += 1
				return nil
			})
		}
		if err := eg.Wait(); err != nil {
			log.Fatalf("failed to roll out tasks due to: %s", err.Error())
			return err
		}
		*rollOutCount += 1
		return nil
	}
}

func (envars *Envars) Rollback(
	awsEcs ecsiface.ECSAPI,
	currentService *ecs.Service,
	nextServiceName string,
	originalTaskCount int,
) error {
	currentTaskCount := int(*currentService.RunningCount)
	rollbackCount := originalTaskCount - currentTaskCount
	if rollbackCount < 0 {
		rollbackCount = currentTaskCount
	}
	log.Infof(
		"start rollback of current service: originalTaskCount=%d, currentTaskCount=%d, tasksToBeRollback=%d",
		originalTaskCount, currentService.RunningCount, rollbackCount,
	)
	rollbackCompletedCount := 0
	rollbackFailedCount := 0
	currentServiceGroup := fmt.Sprintf("service:%s", nextServiceName)
	eg := errgroup.Group{}
	eg.Go(func() error {
		if _, err := awsEcs.DeleteService(&ecs.DeleteServiceInput{
			Cluster: &envars.Cluster,
			Service: &nextServiceName,
		}); err != nil {
			log.Fatalf("failed to delete unhealthy next service due to: %s", err.Error())
			return err
		}
		if err := awsEcs.WaitUntilServicesInactive(&ecs.DescribeServicesInput{
			Cluster:  &envars.Cluster,
			Services: []*string{&nextServiceName},
		}); err != nil {
			log.Fatalf("deleted current service state hasn't reached INACTIVE state within an interval due to: %s", err.Error())
			return err
		}
		return nil
	})
	iconFunc := RunningIcon()
	for i := 0; i < rollbackCount; i++ {
		eg.Go(func() error {
			// タスクを追加
			out, err := awsEcs.StartTask(&ecs.StartTaskInput{
				Cluster:        &envars.Cluster,
				TaskDefinition: currentService.TaskDefinition,
				Group:          &currentServiceGroup,
			})
			if err != nil {
				rollbackFailedCount += 1
				log.Errorf("failed to launch task: taskArn=%s, totalFailure=%d", out.Tasks[0].TaskArn, rollbackFailedCount)
				return err
			}
			if err := awsEcs.WaitUntilTasksRunning(&ecs.DescribeTasksInput{
				Cluster: &envars.Cluster,
				Tasks:   []*string{out.Tasks[0].TaskArn},
			}); err != nil {
				rollbackFailedCount += 1
				log.Errorf("task hasn't reached RUNNING state within maximum attempt windows: taskArn=%s", out.Tasks[0].TaskArn)
				return err
			}
			rollbackCompletedCount += 1
			log.Infof("%s️ rollback is continuing: %d/%d", iconFunc(), rollbackCompletedCount, rollbackCount)
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		//TODO: ここに来たらヤバイので手動ロールバックへの動線を貼る
		log.Fatalf(
			"😱service rollback hasn't completed: succeeded=%d/%d, failed=%d",
			rollbackCompletedCount, rollbackCount, rollbackFailedCount,
		)
		return err
	}
	log.Info("😓service rollback has completed successfully")
	return nil
}
