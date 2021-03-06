package gateway

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/HolmesProcessing/Holmes-Gateway/utils"
	"github.com/streadway/amqp"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

type RabbitConf struct {
	Queue      string
	Exchange   string
	RoutingKey string
}

type config struct {
	HTTP             string
	SourcesKeysPath  string
	TicketKeysPath   string
	SampleStorageURI string
	AllowedTasks     map[string][]string
	RabbitURI        string
	RabbitUser       string
	RabbitPassword   string
	RabbitDefault    RabbitConf
	Rabbit           map[string]RabbitConf
}

var conf *config
var keys map[string]*rsa.PrivateKey
var ticketKeys map[string]*rsa.PublicKey
var keysMutex = &sync.Mutex{}
var rabbitChannel *amqp.Channel
var allowedTasks map[string](map[string]struct{}) // map Organization-Name -> map task

func decryptTicket(enc *tasking.Encrypted) (string, *tasking.MyError, []byte) {
	// Fetch private key corresponding to enc.keyFingerprint
	keysMutex.Lock()
	asymKey, exists := keys[enc.KeyFingerprint]
	keysMutex.Unlock()
	if !exists {
		return "", &tasking.MyError{Error: errors.New("Private key " + enc.KeyFingerprint + " not found"), Code: tasking.ERR_KEY_UNKNOWN}, nil
	}

	// Decrypt symmetric key using the asymmetric key
	symKey, err := tasking.RsaDecrypt(enc.EncryptedKey, asymKey)
	if err != nil {
		return "", &tasking.MyError{Error: err, Code: tasking.ERR_ENCRYPTION}, nil
	}
	//log.Printf("Symmetric Key: %s\n", symKey)

	// Decrypt using the symmetric key
	decrypted, err := tasking.AesDecrypt(enc.Encrypted, symKey, enc.IV)
	if err != nil {
		return string(decrypted), &tasking.MyError{Error: err, Code: tasking.ERR_ENCRYPTION}, symKey
	}
	return string(decrypted), nil, symKey
}

func stringPrintable(s string) bool {
	for i := 0; i < len(s); i++ {
		c := int(s[i])
		if c < 0x9 || (c > 0x0d && c < 0x20) || (c > 0x7e) {
			return false
		}
	}
	return true
}

func checkTask(task *tasking.Task) error {
	log.Printf("Validating %+v\n", task)
	if task.PrimaryURI == "" || !stringPrintable(task.PrimaryURI) {
		return errors.New("Invalid Task (PrimaryURI invalid)")
	}
	if !stringPrintable(task.SecondaryURI) {
		return errors.New("Invalid Task (SecondaryURI invalid)")
	}
	if task.Filename == "" || !stringPrintable(task.Filename) {
		return errors.New("Invalid Task (Filename invalid)")
	}
	if len(task.Tasks) == 0 {
		return errors.New("Invalid Task")
	}
	for k := range task.Tasks {
		if k == "" || !stringPrintable(k) {
			return errors.New("Invalid Task")
		}
	}
	for j := 0; j < len(task.Tags); j++ {
		if !stringPrintable(task.Tags[j]) {
			return errors.New("Invalid Task (Tag invalid)")
		}
	}
	if task.Attempts < 0 {
		return errors.New("Invalid Task (Negative number of attempts)")
	}
	if !stringPrintable(task.Comment) {
		return errors.New("Invalid Task (Comment invalid)")
	}
	return nil
}

func handleDecrypted(ticketStr string) (*tasking.MyError, []tasking.TaskError) {
	tskerrors := make([]tasking.TaskError, 0)
	var ticket tasking.Ticket
	err := json.Unmarshal([]byte(ticketStr), &ticket)
	if err != nil {
		return &tasking.MyError{Error: err, Code: tasking.ERR_OTHER_RECOVERABLE}, tskerrors
	}

	// Check ticket for validity
	signKey, found := ticketKeys[ticket.SignerKeyId]
	if !found {
		return &tasking.MyError{Error: errors.New("Couldn't verify signature: Key unknown"), Code: tasking.ERR_KEY_UNKNOWN}, tskerrors
	}
	err = tasking.VerifyTicket(ticket, signKey)
	if err != nil {
		log.Println("Ticket invalid!")
		return &tasking.MyError{Error: err, Code: tasking.ERR_OTHER_RECOVERABLE}, tskerrors
	}
	log.Println("Signature OK!")
	// Signature is OK

	if time.Now().After(ticket.Expiration) {
		return &tasking.MyError{Error: errors.New("Ticket expired"), Code: tasking.ERR_OTHER_RECOVERABLE}, tskerrors
	}

	// Check ACL
	allowedForOrg, exists := allowedTasks[ticket.SignerKeyId]
	if !exists {
		log.Printf("Organization '%s' not allowed", ticket.SignerKeyId)
		return &tasking.MyError{Error: errors.New("Organization '" + ticket.SignerKeyId + "' not allowed"), Code: tasking.ERR_OTHER_RECOVERABLE}, tskerrors
	}

	// Check for required fields; Check whether strings are in printable ascii-range
	for i := 0; i < len(ticket.Tasks); i++ {
		task := ticket.Tasks[i]
		e := checkTask(&task)
		if e != nil {
			e2 := tasking.MyError{Error: e, Code: tasking.ERR_TASK_INVALID}
			tskerrors = append(tskerrors, tasking.TaskError{
				TaskStruct: task,
				Error:      e2})
		} else {
			// Check whether the corresponding tasks are allowed in ACL:
			acceptedTasks := make(map[string][]string)
			rejectedTasks := make(map[string][]string)

			_, allAllowed := allowedForOrg["*"]
			if allAllowed {
				acceptedTasks = task.Tasks
			} else {
				for tsk, arg := range task.Tasks {
					_, tAllowed := allowedForOrg[tsk]
					if tAllowed {
						acceptedTasks[tsk] = arg
					} else {
						rejectedTasks[tsk] = arg
					}

				}
			}
			log.Printf("Allowed: %+v\n", acceptedTasks)
			log.Printf("Rejected: %+v\n", rejectedTasks)
			savedPrimaryURI := task.PrimaryURI
			savedSecondaryURI := task.SecondaryURI
			task.PrimaryURI = conf.SampleStorageURI + task.PrimaryURI
			if task.SecondaryURI != "" {
				task.SecondaryURI = conf.SampleStorageURI + task.SecondaryURI
			}
			task.Tasks = acceptedTasks
			myerr := pushToTransport(task)
			if myerr != nil {
				task.PrimaryURI = savedPrimaryURI
				task.SecondaryURI = savedSecondaryURI
				task.Tasks = acceptedTasks
				tskerrors = append(tskerrors, tasking.TaskError{
					TaskStruct: task,
					Error:      *myerr})
			}
			if len(rejectedTasks) != 0 {
				task.PrimaryURI = savedPrimaryURI
				task.SecondaryURI = savedSecondaryURI
				task.Tasks = rejectedTasks
				e2 := tasking.MyError{Error: errors.New("Rejected"), Code: tasking.ERR_NOT_ALLOWED}
				tskerrors = append(tskerrors, tasking.TaskError{
					TaskStruct: task,
					Error:      e2})
			}
		}
	}

	return nil, tskerrors
}

func decodeTask(r *http.Request) (*tasking.Encrypted, *tasking.MyError) {
	ek, err := base64.StdEncoding.DecodeString(r.FormValue("EncryptedKey"))
	if err != nil {
		return nil, &tasking.MyError{Error: err, Code: tasking.ERR_OTHER_RECOVERABLE}
	}
	iv, err := base64.StdEncoding.DecodeString(r.FormValue("IV"))
	if err != nil {
		return nil, &tasking.MyError{Error: err, Code: tasking.ERR_OTHER_RECOVERABLE}
	}
	en, err := base64.StdEncoding.DecodeString(r.FormValue("Encrypted"))
	if err != nil {
		return nil, &tasking.MyError{Error: err, Code: tasking.ERR_OTHER_RECOVERABLE}
	}

	task := tasking.Encrypted{
		KeyFingerprint: r.FormValue("KeyFingerprint"),
		EncryptedKey:   ek,
		Encrypted:      en,
		IV:             iv}
	// log.Printf("New task request:\n%+v\n", task);
	return &task, nil
}

func pushToAMQP(task *tasking.Task, rconf *RabbitConf) *tasking.MyError {
	msgBody, err := json.Marshal(task)
	if err != nil {
		log.Println("Error while Marshalling: ", err)
		return &tasking.MyError{Error: err, Code: tasking.ERR_OTHER_RECOVERABLE}
	}
	pub := amqp.Publishing{DeliveryMode: amqp.Persistent, ContentType: "text/plain", Body: msgBody}
	log.Printf("Pushing to %s: \x1b[0;32m%s\x1b[0m\n", rconf.Exchange, msgBody)
	err = rabbitChannel.Publish(rconf.Exchange, rconf.RoutingKey, false, false, pub)

	if err != nil {
		log.Println("Error while pushing to transport: ", err)
		// try to recover three times
		try := 0
		for try < 3 {
			try++
			log.Println("Trying to restore the connection... #", try)
			err = connectRabbit()
			if err == nil {
				break
			}
			// sleep 3 seconds
			time.Sleep(time.Duration(3000000000))
		}
		if err != nil {
			// could not recover the connection after third try => give up
			return &tasking.MyError{Error: err, Code: tasking.ERR_OTHER_RECOVERABLE}
		}
		log.Println("Connection restored")

		// retry pushing
		err = rabbitChannel.Publish(rconf.Exchange, rconf.RoutingKey, false, false, pub)
		if err != nil {
			return &tasking.MyError{Error: err, Code: tasking.ERR_OTHER_RECOVERABLE}
		}
	}
	return nil
}

func pushToTransport(task tasking.Task) *tasking.MyError {
	log.Printf("%+v\n", task)

	// split task:
	tasks := task.Tasks

	// since each task (e.g. CUCKOO, PEID, ...) can have a special destination defined
	// in the config we go trough all tasks in this task struct and check it.
	// If the task had a special destination we cut it out of the original task struct and
	// send it seperately.
	// If the task is sent using RabbitDefault we just leave it in the struct and send the
	// whole task struct after we went trough it completly.
	for t := range tasks {
		log.Println(t)

		// check if special routing is defined in the config
		rconf, exists := conf.Rabbit[t]
		if !exists {
			continue
		}

		// build a seperate task struct
		task.Tasks = map[string][]string{t: tasks[t]}
		if err := pushToAMQP(&task, &rconf); err != nil {
			return err
		}

		// delete the task from the tasks list of the struct
		delete(tasks, t)
	}

	// If there are tasks left we send them all as one big pack to the default destination.
	if len(tasks) == 0 {
		return nil
	}

	task.Tasks = tasks
	if err := pushToAMQP(&task, &conf.RabbitDefault); err != nil {
		return err
	}

	return nil
}

func handleIncoming(task *tasking.Encrypted) (*tasking.MyError, []tasking.TaskError, []byte) {
	decTicket, err, symKey := decryptTicket(task)
	if err != nil {
		log.Println("Error while decrypting: ", err)
		return err, nil, symKey
	}
	log.Println("Decrypted ticket:", decTicket)
	err, tskerrors := handleDecrypted(decTicket)
	if err != nil {
		log.Println("Error: ", err)
		return err, nil, symKey
	}
	// return all the collected errors for individual tasks
	return nil, tskerrors, symKey
}

func httpRequestIncoming(w http.ResponseWriter, r *http.Request) {
	task, err := decodeTask(r)
	if err != nil {
		log.Println("Error while decoding: ", err)
		x, _ := json.Marshal(err)
		w.Write(x)
		return
	}

	err, tskerrors, symKey := handleIncoming(task)
	answer := tasking.GatewayAnswer{
		Error:     err,
		TskErrors: tskerrors,
	}
	// encrypt answer
	task.IV[0] ^= 1 // Do not reuse the same IV -> modify one bit
	x, _ := json.Marshal(answer)
	log.Println("Returning: ", string(x))

	enc, _ := tasking.AesEncrypt(x, symKey, task.IV)
	// TODO: Handle case that symKey could not be extracted
	w.Write(enc)
}

func readKeys() {
	// Load the private keys for the sources
	tasking.LoadKeysAndWatch(conf.SourcesKeysPath, ".priv",
		func(name string) {
			keysMutex.Lock()
			delete(keys, name)
			keysMutex.Unlock()
			log.Println(keys)
		},
		func(name string) {
			key, name, err := tasking.LoadPrivateKey(name)
			if err != nil {
				log.Printf("Error reading key (%s):%s\n", name, err)
				return
			}

			keysMutex.Lock()
			keys[name] = key
			keysMutex.Unlock()
			log.Println(keys)
		})

	// Load the public keys for the tickets
	tasking.LoadKeysAndWatch(conf.TicketKeysPath, ".pub",
		func(name string) {
			keysMutex.Lock()
			delete(ticketKeys, name)
			keysMutex.Unlock()
			log.Println(ticketKeys)
		},
		func(name string) {
			key, name, err := tasking.LoadPublicKey(name)
			if err != nil {
				log.Printf("Error reading key (%s):%s\n", name, err)
				return
			}
			keysMutex.Lock()
			ticketKeys[name] = key
			keysMutex.Unlock()
			log.Println(ticketKeys)
		})

}

func addRabbitConf(r RabbitConf) error {
	queue, err := rabbitChannel.QueueDeclare(
		r.Queue, //name
		true,    // durable
		false,   // delete when unused
		false,   // exclusive
		false,   // no-wait
		nil,     // arguments
	)
	if err != nil {
		return errors.New("Failed to declare a queue: " + err.Error())
	}

	err = rabbitChannel.ExchangeDeclare(
		r.Exchange, // name
		"topic",    // type
		true,       // durable
		false,      // auto-deleted
		false,      // internal
		false,      // no-wait
		nil,        // arguments
	)
	if err != nil {
		return errors.New("Failed to declare an exchange: " + err.Error())
	}

	err = rabbitChannel.QueueBind(
		queue.Name,   // queue name
		r.RoutingKey, // routing key
		r.Exchange,   // exchange
		false,        // nowait
		nil,          // arguments
	)
	if err != nil {
		return errors.New("Failed to bind queue: " + err.Error())
	}
	return nil
}

func connectRabbit() error {
	conn, err := amqp.Dial("amqp://" + conf.RabbitUser + ":" + conf.RabbitPassword + "@" + conf.RabbitURI)
	if err != nil {
		return errors.New("Failed to connect to RabbitMQ: " + err.Error())
	}
	//defer conn.Close()

	rabbitChannel, err = conn.Channel()
	if err != nil {
		return errors.New("Failed to open a channel: " + err.Error())
	}
	//defer rabbitChannel.Close()
	addRabbitConf(conf.RabbitDefault)

	for r := range conf.Rabbit {
		err = addRabbitConf(conf.Rabbit[r])
		if err != nil {
			return err
		}
	}

	log.Println("Connected to Rabbit")
	return nil
}

func initHTTP() {
	http.HandleFunc("/task/", httpRequestIncoming)
	log.Printf("Listening on %s\n", conf.HTTP)
	log.Fatal(http.ListenAndServe(conf.HTTP, nil))
}

func Start(confPath string) {
	conf = &config{}
	cfile, _ := os.Open(confPath)
	err := json.NewDecoder(cfile).Decode(&conf)
	tasking.FailOnError(err, "Couldn't read config file")

	// Parse the private keys
	keys = make(map[string]*rsa.PrivateKey)
	ticketKeys = make(map[string]*rsa.PublicKey)
	readKeys()

	// bring the keys into a map, since this is more
	// efficient in our case
	allowedTasks = make(map[string](map[string]struct{}))
	for org, tasks := range conf.AllowedTasks {
		allowed := make(map[string]struct{})
		for _, t := range tasks {
			// struct{}{} is just an empty placeholder.
			// we are only interested in whether the key exists in the map
			allowed[t] = struct{}{}
		}
		allowedTasks[org] = allowed
	}

	// Connect to rabbitmq
	err = connectRabbit()
	tasking.FailOnError(err, "Failed while connecting to Rabbit")

	// Setup the HTTP-listener
	initHTTP()
}
