package cage

import (
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/aws/aws-sdk-go/service/ecs/ecsiface"
	"regexp"
	"fmt"
	"math"
	"errors"
	"io/ioutil"
	"github.com/apex/log"
	"os"
	"strings"
)

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

func EstimateRollOutCount(originalTaskCount int64) int64 {
	var i int64 = 0
	for ; int64(math.Pow(2, float64(i)))-1 < originalTaskCount; i++ {
	}
	return i
}

func EnsureReplaceCount(
	totalReplacedCount int64,
	totalRollOutCount int64,
	originalCount int64,
) (int64) {
	return int64(math.Min(
		math.Pow(2, float64(totalRollOutCount)),
		float64(originalCount-totalReplacedCount)),
	)
}

func ReadFileAndApplyEnvars(path string) ([]byte, error) {
	if d, err := ioutil.ReadFile(path); err != nil {
		return nil, err
	} else {
		str := string(d)
		reg := regexp.MustCompile("\\${(.+?)}")
		submatches := reg.FindAllStringSubmatch(str, -1)
		for _, m := range submatches {
			if envar, ok := os.LookupEnv(m[1]); ok {
				str = strings.Replace(str, m[0], envar, -1)
			} else {
				log.Warnf("envar literal '%s' found in %s but was not defined. filled by empty string", m[0], path)
				str = strings.Replace(str, m[0], "", -1)
			}
		}
		return []byte(str), nil
	}
}

func NewErrorf(f string, args ...interface{}) error {
	return errors.New(fmt.Sprintf(f, args...))
}

func FindService(
	ecscli ecsiface.ECSAPI,
	cluster *string,
	pattern string,
) (string, error) {
	var services []string
	serviceRegex := regexp.MustCompile("^.+/(.+?)$")
	nameRegex := regexp.MustCompile(pattern)
	if o, err := ecscli.ListServices(&ecs.ListServicesInput{
		Cluster: cluster,
	}); err != nil {
		return "", err
	} else {
		for _, s := range o.ServiceArns {
			if m := serviceRegex.FindStringSubmatch(*s); len(m) > 1 {
				serviceName := m[1]
				if nameRegex.MatchString(serviceName) {
					services = append(services, serviceName)
				}
			}
		}
		if len(services) > 1 {
			return "", NewErrorf(
				"multiple service with prefix '%s' found. cannot determine to roll out: %s", pattern, services,
			)
		} else if len(services) == 0 {
			return "", NewErrorf(
				"no service found that matches '%s' found in '%s'", pattern, *cluster,
			)
		}
		return services[0], nil
	}
}
