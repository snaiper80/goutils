package echomiddleware

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Shopify/sarama"
	"github.com/dgrijalva/jwt-go"
	"github.com/labstack/echo"
	"github.com/pangpanglabs/goutils/behaviorlog"
	"github.com/pangpanglabs/goutils/kafka"
	"github.com/sirupsen/logrus"
)

var (
	passwordRegex      = regexp.MustCompile(`"(password|passwd)":(\s)*"(.*)"`)
	userFieldnameInJwt string
	jwtSecret          = os.Getenv("JWT_SECRET")
)

func init() {
	userFieldnameInJwt = os.Getenv("JWT_USER_FIELDNAME")
	if userFieldnameInJwt == "" {
		userFieldnameInJwt = "userName"
	}
}

func BehaviorLogger(serviceName string, config KafkaConfig) echo.MiddlewareFunc {
	var producer *kafka.Producer
	if p, err := kafka.NewProducer(config.Brokers, config.Topic, func(c *sarama.Config) {
		c.Producer.RequiredAcks = sarama.WaitForLocal       // Only wait for the leader to ack
		c.Producer.Compression = sarama.CompressionGZIP     // Compress messages
		c.Producer.Flush.Frequency = 500 * time.Millisecond // Flush batches every 500ms

	}); err != nil {
		logrus.Error("Create Kafka Producer Error", err)
	} else {
		producer = p
	}

	var echoRouter echoRouter

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) (err error) {
			req := c.Request()

			var body []byte
			if shouldWriteBodyLog(req) {
				body, _ = ioutil.ReadAll(req.Body)
				req.Body.Close()
				req.Body = ioutil.NopCloser(bytes.NewBuffer(body))
			}

			behaviorLogger := behaviorlog.New(serviceName, req, behaviorlog.KafkaProducer(producer))

			c.SetRequest(req.WithContext(context.WithValue(req.Context(),
				behaviorlog.LogContextName, behaviorLogger,
			)))

			if err = next(c); err != nil {
				c.Error(err)
				behaviorLogger.Err = err.Error()
			}

			res := c.Response()

			behaviorLogger.Status = res.Status
			behaviorLogger.BytesSent = res.Size
			behaviorLogger.Controller, behaviorLogger.Action = echoRouter.getControllerAndAction(c)
			if body != nil {
				var bodyParam map[string]interface{}
				d := json.NewDecoder(bytes.NewBuffer(passwordRegex.ReplaceAll(body, []byte(`"$1": "*"`))))
				d.UseNumber()
				if err := d.Decode(&bodyParam); err != nil {
					logrus.WithField("body", string(body)).Error("Decode Request Body Error", err)
				}

				for k, v := range bodyParam {
					behaviorLogger.Params[k] = v
				}
			}

			for _, name := range c.ParamNames() {
				behaviorLogger.Params[name] = c.Param(name)
			}

			behaviorLogger.Username = getUsernameFromJwtToken(req.Header.Get(echo.HeaderAuthorization))

			behaviorLogger.Write()
			return
		}
	}
}

func getUsernameFromJwtToken(auth string) string {
	if !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}

	token, _ := jwt.Parse(auth[len("Bearer "):], func(t *jwt.Token) (interface{}, error) {
		if t.Method.Alg() != "HS256" {
			return nil, fmt.Errorf("unexpected jwt signing method=%v", t.Header["alg"])
		}
		return jwtSecret, nil
	})

	if token == nil || token.Claims == nil {
		return ""
	}

	m, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return ""
	}

	username, ok := m[userFieldnameInJwt]
	if !ok {
		return ""
	}

	if v, ok := username.(string); ok {
		return v
	}

	return ""
}
func shouldWriteBodyLog(req *http.Request) bool {
	if req.Method != http.MethodPost &&
		req.Method != http.MethodPut &&
		req.Method != http.MethodPatch &&
		req.Method != http.MethodDelete {
		return false
	}

	contentType := req.Header.Get(echo.HeaderContentType)
	if contentType != echo.MIMEApplicationJSON &&
		contentType != echo.MIMEApplicationJSONCharsetUTF8 {
		return false
	}

	return true

}

type echoRouter struct {
	once   sync.Once
	routes map[string]string
}

func (er *echoRouter) getControllerAndAction(c echo.Context) (controller, action string) {
	er.once.Do(func() { er.initialize(c) })

	if v := c.Get("controller"); v != nil {
		if controllerName, ok := v.(string); ok {
			controller = controllerName
		}
	}
	if v := c.Get("action"); v != nil {
		if actionName, ok := v.(string); ok {
			action = actionName
		}
	}

	if controller == "" || action == "" {
		handlerName := er.routes[fmt.Sprintf("%s+%s", c.Path(), c.Request().Method)]
		controller, action = er.convertHandlerNameToControllerAndAction(handlerName)
	}
	return
}

func (echoRouter) convertHandlerNameToControllerAndAction(handlerName string) (controller, action string) {
	handlerSplitIndex := strings.LastIndex(handlerName, ".")
	if handlerSplitIndex == -1 || handlerSplitIndex >= len(handlerName) {
		controller, action = "", handlerName
	} else {
		controller, action = handlerName[:handlerSplitIndex], handlerName[handlerSplitIndex+1:]
	}

	// 1. find this pattern: "(controller)"
	controller = controller[strings.Index(controller, "(")+1:]
	if index := strings.Index(controller, ")"); index > 0 {
		controller = controller[:index]
	}
	// 2. remove pointer symbol
	controller = strings.TrimPrefix(controller, "*")
	// 3. split by "/"
	if index := strings.LastIndex(controller, "/"); index > 0 {
		controller = controller[index+1:]
	}

	// remove function symbol
	action = strings.TrimRight(action, ")-fm")
	return
}

func (er *echoRouter) initialize(c echo.Context) {
	er.routes = make(map[string]string)
	for _, r := range c.Echo().Routes() {
		er.routes[fmt.Sprintf("%s+%s", r.Path, r.Method)] = r.Name
	}
}
