package test

import (
	"github.com/aws/aws-sdk-go/service/cloudwatch"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/google/uuid"
	"regexp"
	"fmt"
	"sync"
	"errors"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/apex/log"
	"github.com/aws/aws-sdk-go/service/elbv2"
)

type MockContext struct {
	Services map[string]*ecs.Service
	Tasks    map[string]*ecs.Task
	mux      sync.Mutex
}

func NewMockContext() *MockContext {
	return &MockContext{
		Services: make(map[string]*ecs.Service),
		Tasks:    make(map[string]*ecs.Task),
	}
}

func (ctx *MockContext) GetTask(id string) (*ecs.Task, bool) {
	ctx.mux.Lock()
	defer ctx.mux.Unlock()
	o, ok := ctx.Tasks[id]
	return o, ok
}

func (ctx *MockContext) TaskSize() int64 {
	ctx.mux.Lock()
	defer ctx.mux.Unlock()
	return int64(len(ctx.Tasks))
}

func (ctx *MockContext) GetService(id string) (*ecs.Service, bool) {
	ctx.mux.Lock()
	defer ctx.mux.Unlock()
	o, ok := ctx.Services[id]
	return o, ok
}

func (ctx *MockContext) ServiceSize() int64 {
	ctx.mux.Lock()
	defer ctx.mux.Unlock()
	return int64(len(ctx.Services))
}

func (ctx *MockContext) GetMetricStatics(input *cloudwatch.GetMetricStatisticsInput) (*cloudwatch.GetMetricStatisticsOutput, error) {
	var ret = &cloudwatch.Datapoint{}
	switch *input.MetricName {
	case "RequestCount":
		sum := 100000.0
		ret.Sum = &sum
	case "HTTPCode_ELB_5XX_Count":
		sum := 1.0
		ret.Sum = &sum
	case "HTTPCode_Target_5XX_Count":
		sum := 1.0
		ret.Sum = &sum
	case "TargetResponseTime":
		average := 0.1
		ret.Average = &average
	}
	return &cloudwatch.GetMetricStatisticsOutput{
		Datapoints: []*cloudwatch.Datapoint{ret,},
	}, nil
}

func (ctx *MockContext) CreateService(input *ecs.CreateServiceInput) (*ecs.CreateServiceOutput, error) {
	idstr := uuid.New().String()
	lt := "FARGATE"
	st := "ACTIVE"
	ret := &ecs.Service{
		ServiceName:                   input.ServiceName,
		RunningCount:                  aws.Int64(0),
		LaunchType:                    &lt,
		LoadBalancers:                 input.LoadBalancers,
		DesiredCount:                  input.DesiredCount,
		TaskDefinition:                input.TaskDefinition,
		HealthCheckGracePeriodSeconds: aws.Int64(0),
		Status:                        &st,
		ServiceArn:                    &idstr,
	}
	ctx.mux.Lock()
	ctx.Services[*input.ServiceName] = ret
	ctx.mux.Unlock()
	log.Debugf("%s: running=%d, desired=%d", *input.ServiceName, *ret.RunningCount, *input.DesiredCount)
	for i := 0; i < int(*input.DesiredCount); i++ {
		ctx.StartTask(&ecs.StartTaskInput{
			Cluster:        input.Cluster,
			Group:          aws.String(fmt.Sprintf("service:%s", *input.ServiceName)),
			TaskDefinition: input.TaskDefinition,
		})
	}
	log.Debugf("%s: running=%d", *input.ServiceName, *ret.RunningCount)
	return &ecs.CreateServiceOutput{
		Service: ret,
	}, nil
}

func (ctx *MockContext) UpdateService(input *ecs.UpdateServiceInput) (*ecs.UpdateServiceOutput, error) {
	ctx.mux.Lock()
	s := ctx.Services[*input.Service]
	ctx.mux.Unlock()
	nextDesiredCount := s.DesiredCount
	if input.DesiredCount != nil {
		nextDesiredCount = input.DesiredCount
	}
	if diff := *nextDesiredCount - *s.DesiredCount; diff > 0 {
		log.Debugf("diff=%d", diff)
		// scale
		for i := 0; i < int(diff); i++ {
			ctx.StartTask(&ecs.StartTaskInput{
				Cluster:        input.Cluster,
				Group:          aws.String(fmt.Sprintf("service:%s", *input.Service)),
				TaskDefinition: input.TaskDefinition,
			})
		}
	} else if diff < 0 {
		// descale
		var i int64 = 0
		max := -diff
		for k, v := range ctx.Tasks {
			reg := regexp.MustCompile("service:" + *s.ServiceName)
			if reg.MatchString(*v.Group) {
				ctx.StopTask(&ecs.StopTaskInput{
					Cluster: input.Cluster,
					Task:    &k,
				})
				i++
				if i >= max {
					break
				}
			}
		}
	}
	ctx.mux.Lock()
	s.DesiredCount = nextDesiredCount
	s.TaskDefinition = input.TaskDefinition
	*s.RunningCount = *nextDesiredCount
	ctx.mux.Unlock()
	return &ecs.UpdateServiceOutput{
		Service: s,
	}, nil
}

func (ctx *MockContext) DeleteService(input *ecs.DeleteServiceInput) (*ecs.DeleteServiceOutput, error) {
	ctx.mux.Lock()
	service := ctx.Services[*input.Service]
	ctx.mux.Unlock()
	reg := regexp.MustCompile(fmt.Sprintf("service:%s", *service.ServiceName))
	for _, v := range ctx.Tasks {
		if reg.MatchString(*v.Group) {
			_, err := ctx.StopTask(&ecs.StopTaskInput{
				Cluster: input.Cluster,
				Task:    v.TaskArn,
			})
			if err != nil {
				return nil, err
			}
		}
	}
	ctx.mux.Lock()
	defer ctx.mux.Unlock()
	delete(ctx.Services, *input.Service)
	return &ecs.DeleteServiceOutput{
		Service: service,
	}, nil
}

func (ctx *MockContext) RegisterTaskDefinition(input *ecs.RegisterTaskDefinitionInput) (*ecs.RegisterTaskDefinitionOutput, error) {
	idstr := uuid.New().String()
	return &ecs.RegisterTaskDefinitionOutput{
		TaskDefinition: &ecs.TaskDefinition{
			TaskDefinitionArn: &idstr,
			Family: aws.String("family"),
			Revision: aws.Int64(1),
		},
	}, nil
}

func (ctx *MockContext) StartTask(input *ecs.StartTaskInput) (*ecs.StartTaskOutput, error) {
	regex := regexp.MustCompile("service:(.+?)$")
	m := regex.FindStringSubmatch(*input.Group)
	id := uuid.New()
	idstr := id.String()
	ret := &ecs.Task{
		TaskArn:           &idstr,
		ClusterArn:        input.Cluster,
		TaskDefinitionArn: input.TaskDefinition,
		Group:             input.Group,
		Attachments: []*ecs.Attachment {{
			Details: []*ecs.KeyValuePair{{
				Name: aws.String("privateIPv4Address"),
				Value: aws.String("127.0.0.1"),
			}},
		}},
		LaunchType: aws.String("FARGATE"),
	}
	ctx.mux.Lock()
	defer ctx.mux.Unlock()
	ctx.Tasks[idstr] = ret
	s := ctx.Services[m[1]]
	*s.RunningCount += 1
	return &ecs.StartTaskOutput{
		Tasks: []*ecs.Task{ret},
	}, nil
}

func (ctx *MockContext) StopTask(input *ecs.StopTaskInput) (*ecs.StopTaskOutput, error) {
	ctx.mux.Lock()
	defer ctx.mux.Unlock()
	log.Debugf("stop: %s", input)
	ret := ctx.Tasks[*input.Task]
	delete(ctx.Tasks, *input.Task)
	reg := regexp.MustCompile("service:(.+?)$")
	m := reg.FindStringSubmatch(*ret.Group)
	service := ctx.Services[m[1]]
	*service.RunningCount -= 1
	return &ecs.StopTaskOutput{
		Task: ret,
	}, nil
}

func (ctx *MockContext) ListTasks(input *ecs.ListTasksInput) (*ecs.ListTasksOutput, error) {
	var ret []*string
	ctx.mux.Lock()
	defer ctx.mux.Unlock()
	for _, v := range ctx.Tasks {
		group := fmt.Sprintf("service:%s", *input.ServiceName)
		if *v.Group == group {
			ret = append(ret, v.TaskArn)
		}
	}
	return &ecs.ListTasksOutput{
		TaskArns: ret,
	}, nil
}

func (ctx *MockContext) WaitUntilServicesStable(input *ecs.DescribeServicesInput) (error) {
	ctx.mux.Lock()
	defer ctx.mux.Unlock()
	for _, v := range input.Services {
		if _, ok := ctx.Services[*v]; !ok {
			return errors.New(fmt.Sprintf("service:%s not found", *v))
		}
	}
	return nil
}

func (ctx *MockContext) DescribeServices(input *ecs.DescribeServicesInput) (*ecs.DescribeServicesOutput, error) {
	var ret []*ecs.Service
	ctx.mux.Lock()
	defer ctx.mux.Unlock()
	for _, v := range input.Services {
		if s, ok := ctx.Services[*v]; ok {
			ret = append(ret, s)
		}
	}
	return &ecs.DescribeServicesOutput{
		Services: ret,
	}, nil
}

func (ctx *MockContext) WaitUntilServicesInactive(input *ecs.DescribeServicesInput) (error) {
	ctx.mux.Lock()
	defer ctx.mux.Unlock()
	for _, v := range input.Services {
		if _, ok := ctx.Services[*v]; ok {
			return errors.New(fmt.Sprintf("service:%s found", *v))
		}
	}
	return nil
}

func (ctx *MockContext) WaitUntilTasksRunning(input *ecs.DescribeTasksInput) (error) {
	ctx.mux.Lock()
	defer ctx.mux.Unlock()
	for _, v := range input.Tasks {
		if _, ok := ctx.Tasks[*v]; !ok {
			return errors.New(fmt.Sprintf("task:%s not running", *v))
		}
	}
	return nil
}
func (ctx *MockContext) WaitUntilTasksStopped(input *ecs.DescribeTasksInput) (error) {
	ctx.mux.Lock()
	defer ctx.mux.Unlock()
	for _, v := range input.Tasks {
		if _, ok := ctx.Tasks[*v]; ok {
			return errors.New(fmt.Sprintf("task:%s found", *v))
		}
	}
	return nil
}
func (ctx *MockContext) DescribeTasks(input *ecs.DescribeTasksInput) (*ecs.DescribeTasksOutput, error) {
	ctx.mux.Lock()
	defer ctx.mux.Unlock()
	var ret []*ecs.Task
	for _, task := range ctx.Tasks {
		for _, v := range input.Tasks {
			if *task.TaskArn == *v {
				ret = append(ret, task)
			}
		}
	}
	return &ecs.DescribeTasksOutput{
		Tasks: ret,
	}, nil
}

//

func (ctx *MockContext) DescribeTargetGroups(input *elbv2.DescribeTargetGroupsInput) (*elbv2.DescribeTargetGroupsOutput, error) {
	return &elbv2.DescribeTargetGroupsOutput{
		TargetGroups: []*elbv2.TargetGroup{
			{
				TargetGroupName:            aws.String("tgname"),
				TargetGroupArn:             input.TargetGroupArns[0],
				HealthyThresholdCount:      aws.Int64(1),
				HealthCheckIntervalSeconds: aws.Int64(0),
				LoadBalancerArns:           []*string{aws.String("arn://hoge/app/aa/bb")},
			},
		},
	}, nil
}
func (ctx *MockContext) DescribeTargetGroupAttibutes(input *elbv2.DescribeTargetGroupAttributesInput) (*elbv2.DescribeTargetGroupAttributesOutput, error) {
	return &elbv2.DescribeTargetGroupAttributesOutput{
		Attributes: []*elbv2.TargetGroupAttribute{
			{
				Key:   aws.String("deregistration_delay.timeout_seconds"),
				Value: aws.String("0"),
			},
		},
	}, nil
}
func (ctx *MockContext) DescribeTargetHealth(input *elbv2.DescribeTargetHealthInput) (*elbv2.DescribeTargetHealthOutput, error) {
	var ret []*elbv2.TargetHealthDescription
	for i := int64(0); i < ctx.TaskSize(); i++ {
		ret = append(ret, &elbv2.TargetHealthDescription{
			Target: &elbv2.TargetDescription{
				Id: aws.String("127.0.0.1"),
				Port: aws.Int64(8000),
				AvailabilityZone: aws.String("us-west-2"),
			},
			TargetHealth: &elbv2.TargetHealth{
				State: aws.String("healthy"),
			},
		})
	}
	return &elbv2.DescribeTargetHealthOutput{
		TargetHealthDescriptions: ret,
	}, nil
}
