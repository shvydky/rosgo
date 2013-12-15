package ros

import (
    "container/list"
    "encoding/binary"
    "encoding/hex"
    "errors"
    "net"
    "sync"
    "time"
)

type defaultPublisher struct {
    logger            Logger
    nodeId            string
    nodeApiUri        string
    masterUri         string
    topic             string
    msgType           MessageType
    msgChan           chan []byte
    shutdownChan      chan struct{}
    sessions          *list.List
    listenerErrorChan chan error
    sessionErrorChan  chan error
    listener          net.Listener
}

func newDefaultPublisher(logger Logger, nodeId string, nodeApiUri string, masterUri string, topic string, msgType MessageType) *defaultPublisher {
    pub := new(defaultPublisher)
    pub.logger = logger
    pub.nodeId = nodeId
    pub.nodeApiUri = nodeApiUri
    pub.masterUri = masterUri
    pub.topic = topic
    pub.msgType = msgType
    pub.shutdownChan = make(chan struct{})
    pub.sessions = list.New()
    pub.msgChan = make(chan []byte, 10)
    pub.listenerErrorChan = make(chan error)
    if listener, err := listenRandomPort("127.0.0.1", 10); err != nil {
        panic(err)
    } else {
        pub.listener = listener
    }
    return pub
}

func (pub *defaultPublisher) start(wg *sync.WaitGroup) {
    logger := pub.logger
    logger.Debugf("Publisher goroutine for %s started.", pub.topic)
    wg.Add(1)
    defer wg.Done()

    go pub.listenRemoteSubscriber()

    for {
        select {
        case msg := <-pub.msgChan:
            logger.Debug("Receive msgChan")
            for e := pub.sessions.Front(); e != nil; e = e.Next() {
                session := e.Value.(*remoteSubscriberSession)
                session.msgChan <- msg
            }
        case err := <-pub.listenerErrorChan:
            logger.Debug("Listener closed unexpectedly: %s", err)
            pub.listener.Close()
            return
        case session, err := <-pub.sessionErrorChan:
            logger.Error(err)
            for e := pub.sessions.Front(); e != nil; e = e.Next() {
                if e.Value == session {
                    pub.sessions.Remove(e)
                    break
                }
            }
        case <-pub.shutdownChan:
            logger.Debug("Receive shutdownChan")
            pub.listener.Close()
            _, err := callRosApi(pub.masterUri, "unregisterPublisher", pub.nodeId, pub.topic, pub.nodeApiUri)
            if err != nil {
                logger.Warn(err)
            }
            for e := pub.sessions.Front(); e != nil; e = e.Next() {
                session := e.Value.(*remoteSubscriberSession)
                session.quitChan <- struct{}{}
            }
            pub.sessions.Init() // Clear all sessions
            return
        }
    }
}

func (pub *defaultPublisher) listenRemoteSubscriber() {
    logger := pub.logger
    logger.Debugf("Start listen %s.", pub.listener.Addr().String())
    for {
        if conn, err := pub.listener.Accept(); err != nil {
            pub.listenerErrorChan <- err
            return
        } else {
            logger.Debugf("Connected %s", conn.RemoteAddr().String())
            session := newRemoteSubscriberSession(pub, conn)
            pub.sessions.PushBack(session)
            go session.start()
        }
    }
}

func (pub *defaultPublisher) Publish(msg Message) {
    pub.msgChan <- msg.Serialize()
}

func (pub *defaultPublisher) Shutdown() {
    pub.shutdownChan <- struct{}{}
}

func (pub *defaultPublisher) hostAndPort() (string, string) {
    addr, port, err := net.SplitHostPort(pub.listener.Addr().String())
    if err != nil {
        // Not reached
        panic(err)
    }
    return addr, port
}

type remoteSubscriberSession struct {
    conn      net.Conn
    nodeId    string
    topic     string
    md5sum    string
    typeName  string
    quitChan  chan struct{}
    msgChan   chan []byte
    errorChan chan error
    logger    Logger
}

func newRemoteSubscriberSession(pub *defaultPublisher, conn net.Conn) *remoteSubscriberSession {
    session := new(remoteSubscriberSession)
    session.conn = conn
    session.nodeId = pub.nodeId
    session.topic = pub.topic
    session.md5sum = pub.msgType.MD5Sum()
    session.typeName = pub.msgType.Name()
    session.quitChan = make(chan struct{})
    session.msgChan = make(chan []byte, 10)
    session.errorChan = pub.sessionErrorChan
    session.logger = pub.logger
    return session
}

func (session *remoteSubscriberSession) start() {
    logger := session.logger
    defer func() {
        if err := recover(); err != nil {
            if e, ok := err.(error); ok {
                session.errorChan <- e
            } else {
                logger.Error("Unkonwn error value")
            }
        }
    }()
    // 1. Read connection header
    headers, err := readConnectionHeader(session.conn)
    if err != nil {
        panic(errors.New("Failed to read connection header."))
    }
    logger.Debug("TCPROS Connection Header:")
    headerMap := make(map[string]string)
    for _, h := range headers {
        headerMap[h.key] = h.value
        logger.Debugf("  `%s` = `%s`", h.key, h.value)
    }
    if headerMap["type"] != session.typeName || headerMap["md5sum"] != session.md5sum {
        panic(errors.New("Incomatible message type!"))
    }

    // 2. Return reponse header
    var resHeaders []header
    resHeaders = append(resHeaders, header{"md5sum", session.md5sum})
    resHeaders = append(resHeaders, header{"type", session.typeName})
    logger.Debug("TCPROS Response Header")
    for _, h := range resHeaders {
        logger.Debugf("  `%s` = `%s`", h.key, h.value)
    }
    err = writeConnectionHeader(resHeaders, session.conn)
    if err != nil {
        panic(errors.New("Failed to write response header."))
    }

    // 3. Start sending message
    logger.Debug("Start sending messages...")
    queue := list.New()
    queueMaxSize := 100
    for {
        select {
        case msg := <-session.msgChan:
            logger.Debug("Receive msgChan")
            if queue.Len() == queueMaxSize {
                queue.Remove(queue.Front())
            }
            queue.PushBack(msg)
        case <-session.quitChan:
            logger.Debug("Receive quitChan")
            return
        case <-time.After(10 * time.Millisecond):
            if queue.Len() > 0 {
                logger.Debug("writing")
                msg := queue.Front().Value.([]byte)
                queue.Remove(queue.Front())
                logger.Debug(hex.EncodeToString(msg))
                session.conn.SetDeadline(time.Now().Add(10 * time.Millisecond))
                size := uint32(len(msg))
                if err := binary.Write(session.conn, binary.LittleEndian, size); err != nil {
                    if neterr, ok := err.(net.Error); ok && neterr.Timeout() {
                        logger.Debug("timeout")
                        continue
                    } else {
                        logger.Error(err)
                        panic(err)
                    }
                }
                logger.Debug(len(msg))
                session.conn.SetDeadline(time.Now().Add(10 * time.Millisecond))
                if _, err := session.conn.Write(msg); err != nil {
                    if neterr, ok := err.(net.Error); ok && neterr.Timeout() {
                        logger.Debug("timeout")
                        continue
                    } else {
                        logger.Error(err)
                        panic(err)
                    }
                }
                logger.Debug(hex.EncodeToString(msg))
            }
        }
    }
}