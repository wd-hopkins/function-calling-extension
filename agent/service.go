package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/copilot-extensions/function-calling-extension/copilot"
	"github.com/google/go-github/v57/github"
	"github.com/invopop/jsonschema"
	"github.com/wk8/go-ordered-map/v2"
	"io"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"math/big"
	"net/http"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

var tools []copilot.FunctionTool

func init() {
	listPodProperties := orderedmap.New[string, *jsonschema.Schema]()
	listPodProperties.Set("namespace", &jsonschema.Schema{
		Type:        "string",
		Description: "The namespace to list pods from",
	})
	deleteNamespaceProperties := orderedmap.New[string, *jsonschema.Schema]()
	deleteNamespaceProperties.Set("namespace", &jsonschema.Schema{
		Type:        "string",
		Description: "The namespace to list pods from",
	})

	tools = []copilot.FunctionTool{
		{
			Type: "function",
			Function: copilot.Function{
				Name:        "get_namespaces",
				Description: "Fetch all of the namespaces from a kubernetes cluster. When responding with a list, use a bullet pointed list. A user may ask for all namespaces, or a specific namespace. A user may ask for information about a namespace. Use the fields in the namespace object to determine what information to provide.",
				Parameters: &jsonschema.Schema{
					Type:       "object",
					Properties: orderedmap.New[string, *jsonschema.Schema](),
					Required:   []string{},
				},
			},
		},
		{
			Type: "function",
			Function: copilot.Function{
				Name:        "delete_namespace",
				Description: "A user will provide a namespace to delete. The input is the namespace name. Prompt the user to confirm the deletion.",
				Parameters: &jsonschema.Schema{
					Type:       "object",
					Properties: deleteNamespaceProperties,
					Required:   []string{"namespace"},
				},
			},
		},
		{
			Type: "function",
			Function: copilot.Function{
				Name:        "get_pods_in_namespace",
				Description: "Fetch a list of pods from a kubernetes namespace. The input is the namespace name. When responding with a list, use a bullet pointed list. A user may ask for all pods, or a specific pod. A user may ask for information about a pod. Only give the name of the pod unless asked otherwise. Use the fields in the pod object to determine what information to provide.",
				Parameters: &jsonschema.Schema{
					Type:       "object",
					Properties: listPodProperties,
					Required:   []string{"namespace"},
				},
			},
		},
	}
}

// Service provides and endpoint for this agent to perform chat completions
type Service struct {
	pubKey *ecdsa.PublicKey
}

func NewService(pubKey *ecdsa.PublicKey) *Service {
	return &Service{
		pubKey: pubKey,
	}
}

func (s *Service) ChatCompletion(w http.ResponseWriter, r *http.Request) {
	sig := r.Header.Get("Github-Public-Key-Signature")

	body, err := io.ReadAll(r.Body)
	if err != nil {
		fmt.Println(fmt.Errorf("failed to read request body: %w", err))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// Make sure the payload matches the signature. In this way, you can be sure
	// that an incoming request comes from github
	isValid, err := validPayload(body, sig, s.pubKey)
	if err != nil {
		fmt.Printf("failed to validate payload signature: %v\n", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if !isValid {
		http.Error(w, "invalid payload signature", http.StatusUnauthorized)
		return
	}

	apiToken := r.Header.Get("X-GitHub-Token")
	integrationID := r.Header.Get("Copilot-Integration-Id")

	var req *copilot.ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		fmt.Printf("failed to unmarshal request: %v\n", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if err := generateCompletion(r.Context(), integrationID, apiToken, req, NewSSEWriter(w)); err != nil {
		fmt.Printf("failed to execute agent: %v\n", err)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func generateCompletion(ctx context.Context, integrationID, apiToken string, req *copilot.ChatRequest, w *sseWriter) error {
	// If the user clicks a confirmation box, handle that, and ignore everything else.
	for _, conf := range req.Messages[len(req.Messages)-1].Confirmations {
		if conf.State != "accepted" {
			continue
		}

		err := deleteNamespace(ctx, conf.Confirmation.Namespace)
		if err != nil {
			return err
		}

		w.writeData(sseResponse{
			Choices: []sseResponseChoice{
				{
					Index: 0,
					Delta: sseResponseMessage{
						Role:    "assistant",
						Content: fmt.Sprintf("Namespace \"%s\" deleted", conf.Confirmation.Namespace),
					},
				},
			},
		})

		return nil
	}

	var messages []copilot.ChatMessage
	var confs []copilot.ConfirmationData
	messages = append(messages, req.Messages...)

	// The LLM will call functions when it wants more information.  We loop here in
	// order to feed that information back into the LLM
	for i := 0; i < 5; i++ {

		chatReq := &copilot.ChatCompletionsRequest{
			Model:    copilot.ModelGPT35,
			Messages: messages,
		}

		// Only give the LLM tools if we're not on the last loop.  On the final
		// iteration, we want to force a chat completion out of the LLM
		if i < 4 {
			chatReq.Tools = tools
		}

		res, err := copilot.ChatCompletions(ctx, integrationID, apiToken, chatReq)
		if err != nil {
			return fmt.Errorf("failed to get chat completions stream: %w", err)
		}

		function := getFunctionCall(res)

		// If there's no function call, send the completion back to the client
		if function == nil {
			choices := make([]sseResponseChoice, len(res.Choices))
			for i, choice := range res.Choices {
				choices[i] = sseResponseChoice{
					Index: choice.Index,
					Delta: sseResponseMessage{
						Role:    choice.Message.Role,
						Content: choice.Message.Content,
					},
				}
			}

			w.writeData(sseResponse{
				Choices: choices,
			})
			w.writeDone()
			break
		}

		fmt.Println("found function!", function.Name)

		switch function.Name {

		case "get_namespaces":
			msg, err := getNamespaces(ctx)
			if err != nil {
				return err
			}
			messages = append(messages, *msg)
		case "get_pods_in_namespace":
			args := &struct {
				Namespace string `json:"namespace"`
			}{}
			err := json.Unmarshal([]byte(function.Arguments), &args)
			if err != nil {
				return fmt.Errorf("error unmarshalling function arguments: %w", err)
			}
			msg, err := getPodsInNamespace(ctx, args.Namespace)
			if err != nil {
				return err
			}
			messages = append(messages, *msg)
		case "delete_namespace":
			args := &struct {
				Namespace string `json:"namespace"`
			}{}
			err := json.Unmarshal([]byte(function.Arguments), &args)
			if err != nil {
				return fmt.Errorf("error unmarshalling function arguments: %w", err)
			}

			conf, msg := deleteNamespaceConfirmation(args.Namespace)

			found := false
			for _, existing_conf := range confs {
				if *conf.Confirmation == existing_conf {
					found = true
					break
				}
			}

			if !found {
				confs = append(confs, *conf.Confirmation)

				if err := w.writeEvent("copilot_confirmation"); err != nil {
					return fmt.Errorf("failed to write event: %w", err)
				}

				if err := w.writeData(conf); err != nil {
					return fmt.Errorf("failed to write data: %w", err)
				}

				messages = append(messages, *msg)
			}
		default:
			return fmt.Errorf("unknown function call: %s", function.Name)
		}
	}
	return nil
}

func getNamespaces(ctx context.Context) (*copilot.ChatMessage, error) {
	k8sClient, err := getLocalClient()
	if err != nil {
		return nil, fmt.Errorf("error getting k8s client: %w", err)
	}

	namespaceList := &corev1.NamespaceList{}
	err = k8sClient.List(ctx, namespaceList)
	if err != nil {
		return nil, fmt.Errorf("error listing namespaces: %w", err)
	}

	serializedNamespaces, err := json.Marshal(namespaceList)
	if err != nil {
		return nil, fmt.Errorf("error serializing namespaces")
	}

	return &copilot.ChatMessage{
		Role:    "system",
		Content: fmt.Sprintf("The namespaces in the cluster are: %s", string(serializedNamespaces)),
	}, nil
}

func deleteNamespace(ctx context.Context, ns string) error {
	k8sClient, err := getLocalClient()
	if err != nil {
		return fmt.Errorf("error getting k8s client: %w", err)
	}

	namespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: ns,
		},
	}
	err = k8sClient.Delete(ctx, namespace)
	if err != nil {
		return fmt.Errorf("error deleting namespace: %w", err)
	}

	return nil
}

func getPodsInNamespace(ctx context.Context, namespace string) (*copilot.ChatMessage, error) {
	k8sClient, err := getLocalClient()
	if err != nil {
		return nil, fmt.Errorf("error getting k8s client: %w", err)
	}

	podList := &corev1.PodList{}
	err = k8sClient.List(ctx, podList, client.InNamespace(namespace))
	if err != nil {
		return nil, fmt.Errorf("error listing namespaces: %w", err)
	}

	serializedPods, err := json.Marshal(podList)
	if err != nil {
		return nil, fmt.Errorf("error serializing pods")
	}

	return &copilot.ChatMessage{
		Role:    "system",
		Content: fmt.Sprintf("The pods in the namespace %s are: %s", namespace, string(serializedPods)),
	}, nil
}

func getLocalClient() (client.Client, error) {
	kc, err := config.GetConfig()
	if err != nil {
		return nil, err
	}

	newKubeClient, err := client.New(kc, client.Options{
		Scheme: scheme.Scheme,
	})
	if err != nil {
		return nil, err
	}

	return newKubeClient, nil
}

func deleteNamespaceConfirmation(ns string) (*copilot.ResponseConfirmation, *copilot.ChatMessage) {
	return &copilot.ResponseConfirmation{
			Type:    "action",
			Title:   "Delete Namespace",
			Message: fmt.Sprintf("Are you sure you want to delete the namespace \"%s\"", ns),
			Confirmation: &copilot.ConfirmationData{
				Namespace: ns,
			},
		}, &copilot.ChatMessage{
			Role:    "system",
			Content: fmt.Sprintf("Namespace \"%s\" deleted", ns),
		}
}

func createIssue(ctx context.Context, apiToken, owner, repo, title, body string) error {
	client := github.NewClient(nil).WithAuthToken(apiToken)
	_, _, err := client.Issues.Create(ctx, owner, repo, &github.IssueRequest{
		Title: &title,
		Body:  &body,
	})
	if err != nil {
		return fmt.Errorf("error creating issue: %w", err)
	}

	return nil
}

// asn1Signature is a struct for ASN.1 serializing/parsing signatures.
type asn1Signature struct {
	R *big.Int
	S *big.Int
}

func validPayload(data []byte, sig string, publicKey *ecdsa.PublicKey) (bool, error) {
	asnSig, err := base64.StdEncoding.DecodeString(sig)
	parsedSig := asn1Signature{}
	if err != nil {
		return false, err
	}
	rest, err := asn1.Unmarshal(asnSig, &parsedSig)
	if err != nil || len(rest) != 0 {
		return false, err
	}

	// Verify the SHA256 encoded payload against the signature with GitHub's Key
	digest := sha256.Sum256(data)
	return ecdsa.Verify(publicKey, digest[:], parsedSig.R, parsedSig.S), nil
}

func getFunctionCall(res *copilot.ChatCompletionsResponse) *copilot.ChatMessageFunctionCall {
	if len(res.Choices) == 0 {
		return nil
	}

	if len(res.Choices[0].Message.ToolCalls) == 0 {
		return nil
	}

	funcCall := res.Choices[0].Message.ToolCalls[0].Function
	if funcCall == nil {
		return nil
	}
	return funcCall

}
