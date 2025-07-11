package main

import (
	"io"
	"log"
	"net/url"
	"strings"

	"net/http"
	"os"

	"go.uber.org/zap"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/sqs"

	"github.com/fission/keda-connectors/common"
)

type awsSQSConnector struct {
	sqsURL        *url.URL
	sqsClient     *sqs.SQS
	connectordata common.ConnectorMetadata
	logger        *zap.Logger
}

func parseURL(baseURL *url.URL, queueName string) (string, error) {
	u, err := url.Parse(queueName)
	if err != nil {
		return "", err
	}
	consQueueURL := baseURL.ResolveReference(u)
	return consQueueURL.String(), nil
}

func (conn awsSQSConnector) consumeMessage() {
	var maxNumberOfMessages = int64(10) // Process maximum 10 messages concurrently
	var waitTimeSeconds = int64(5)      // Wait 5 sec to process another message
	var respQueueURL, errorQueueURL string
	headers := http.Header{
		"KEDA-Topic":          {conn.connectordata.Topic},
		"KEDA-Response-Topic": {conn.connectordata.ResponseTopic},
		"KEDA-Error-Topic":    {conn.connectordata.ErrorTopic},
		"Content-Type":        {conn.connectordata.ContentType},
		"KEDA-Source-Name":    {conn.connectordata.SourceName},
	}

	consQueueURL, err := parseURL(conn.sqsURL, os.Getenv("TOPIC"))
	if err != nil {
		conn.logger.Error("failed to parse consumer queue url", zap.Error(err))
	}

	if os.Getenv("RESPONSE_TOPIC") != "" {
		respQueueURL, err = parseURL(conn.sqsURL, os.Getenv("RESPONSE_TOPIC"))
		if err != nil {
			conn.logger.Error("failed to parse response queue url", zap.Error(err))
		}
	}

	if os.Getenv("ERROR_TOPIC") != "" {
		errorQueueURL, err = parseURL(conn.sqsURL, os.Getenv("ERROR_TOPIC"))
		if err != nil {
			conn.logger.Error("failed to parse error queue url", zap.Error(err))
		}
	}

	conn.logger.Info("starting to consume messages from queue", zap.String("queue", consQueueURL), zap.String("response queue", respQueueURL), zap.String("error queue", errorQueueURL))

	for {
		output, err := conn.sqsClient.ReceiveMessage(&sqs.ReceiveMessageInput{
			QueueUrl:            &consQueueURL,
			MaxNumberOfMessages: &maxNumberOfMessages,
			WaitTimeSeconds:     &waitTimeSeconds,
		})

		if err != nil {
			conn.logger.Error("failed to fetch sqs message", zap.Error(err))
		}

		for _, message := range output.Messages {
			// Set the attributes as message header came from SQS record
			for k, v := range message.Attributes {
				headers.Add(k, *v)
			}

			resp, err := common.HandleHTTPRequest(*message.Body, headers, conn.connectordata, conn.logger)
			if err != nil {
				conn.errorHandler(errorQueueURL, err)
			} else {
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					conn.errorHandler(errorQueueURL, err)
				} else {
					// Generating SQS Message attribute
					var sqsMessageAttValue = make(map[string]*sqs.MessageAttributeValue)
					for k, v := range resp.Header {
						for _, d := range v {
							sqsMessageAttValue[k] = &sqs.MessageAttributeValue{
								DataType:    aws.String("String"),
								StringValue: aws.String(d),
							}
						}
					}
					if success := conn.responseHandler(respQueueURL, string(body), sqsMessageAttValue); success {
						conn.deleteMessage(*message.ReceiptHandle, consQueueURL)
					}
				}
				err = resp.Body.Close()
				if err != nil {
					conn.logger.Error("failed to close response body", zap.Error(err))
				}
			}
		}
	}
}

func (conn awsSQSConnector) responseHandler(queueURL string, response string, messageAttValue map[string]*sqs.MessageAttributeValue) bool {
	if queueURL != "" {
		_, err := conn.sqsClient.SendMessage(&sqs.SendMessageInput{
			DelaySeconds:      aws.Int64(10),
			MessageAttributes: messageAttValue,
			MessageBody:       &response,
			QueueUrl:          &queueURL,
		})
		if err != nil {
			conn.logger.Error("failed to publish response body from http request to topic",
				zap.Error(err),
				zap.String("topic", conn.connectordata.ResponseTopic),
				zap.String("source", conn.connectordata.SourceName),
				zap.String("http endpoint", conn.connectordata.HTTPEndpoint),
			)
			return false
		}
	} else {
		conn.logger.Debug("response received", zap.String("response", response))
	}
	return true
}

func (conn *awsSQSConnector) errorHandler(queueURL string, err error) {
	if queueURL != "" {
		errMsg := err.Error()
		_, err := conn.sqsClient.SendMessage(&sqs.SendMessageInput{
			DelaySeconds: aws.Int64(10),
			//MessageAttributes: messageAttValue,
			MessageBody: &errMsg,
			QueueUrl:    &queueURL,
		})
		if err != nil {
			conn.logger.Error("failed to publish message to error topic",
				zap.Error(err),
				zap.String("source", conn.connectordata.SourceName),
				zap.String("message", err.Error()),
				zap.String("topic", conn.connectordata.ErrorTopic))
		}
	} else {
		conn.logger.Error("message received to publish to error topic, but no error topic was set",
			zap.String("message", err.Error()),
			zap.String("source", conn.connectordata.SourceName),
			zap.String("http endpoint", conn.connectordata.HTTPEndpoint),
		)
	}
}

func (conn *awsSQSConnector) deleteMessage(id string, queueURL string) {
	_, err := conn.sqsClient.DeleteMessage(&sqs.DeleteMessageInput{
		QueueUrl:      &queueURL,
		ReceiptHandle: &id,
	})

	if err != nil {
		conn.logger.Error("delete Error", zap.Error(err))
		return
	}

	conn.logger.Info("message deleted")
}

func main() {
	logger, err := zap.NewProduction()
	if err != nil {
		log.Fatalf("can't initialize zap logger: %v", err)
	}
	defer logger.Sync()

	connectordata, err := common.ParseConnectorMetadata()
	if err != nil {
		logger.Fatal("failed to parse connector metadata", zap.Error(err))
	}
	config, err := common.GetAwsConfig()
	if err != nil {
		logger.Error("failed to fetch aws config", zap.Error(err))
		return
	}

	sess, err := common.CreateValidatedSession(config)
	if err != nil {
		logger.Error("not able create session using aws configuration", zap.Error(err))
		return
	}
	svc := sqs.New(sess)

	sqsURL, err := url.Parse(strings.TrimSuffix(os.Getenv("QUEUE_URL"), os.Getenv("TOPIC")))
	if err != nil {
		logger.Error("not able parse aws sqs url", zap.Error(err))
		return
	}

	conn := awsSQSConnector{
		sqsURL:        sqsURL,
		sqsClient:     svc,
		connectordata: connectordata,
		logger:        logger,
	}
	conn.consumeMessage()
}
