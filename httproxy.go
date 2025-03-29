package main

import (
    "bytes"
    "flag"
    "fmt"
    "io"
    "log"
    "net/http"
    "net/http/httputil"
    "net/url"
    "os"
    "regexp"
    "strconv"
    "strings"
	"encoding/hex"
)

type Config struct {
    listenPort string
    targetURL  string
	debugLevel int
	delimiter  string	
    reqHeader  []string
    reqBody    []string
    respHeader []string
    respBody   []string
}

type RegexReplacement struct {
	id          int
    Pattern     *regexp.Regexp
    Replacement string
    Count       int // -1 表示替换所有匹配项
    HeaderKey   string
}

var (
    config           Config
    reqHeaderRules   []RegexReplacement
    reqBodyRules     []RegexReplacement
    respHeaderRules  []RegexReplacement
    respBodyRules    []RegexReplacement
)

func customUsage() {
    fmt.Fprintf(os.Stderr, "\n")
    fmt.Fprintf(os.Stderr, "简单HTTP反向代理\n用法：%s [参数...]\n", os.Args[0])
    flag.PrintDefaults()
    fmt.Fprintf(os.Stderr, "\n示例:\n  %s -listen :8080 -target https://wishub-x1.ctyun.cn -req-body \"\\\"stream\\\":?true::\\\"stream\\\": false\" -resp-body \"\\\"message\\\"::\\\"delta\\\"\"\n", os.Args[0])
}

func main() {
    flag.Usage = customUsage
	flag.IntVar(&config.debugLevel,"debug", 2, "输出debug信息")
    flag.StringVar(&config.delimiter,"delim", "\n\n", "流模式chunk分隔符")
    flag.StringVar(&config.listenPort, "listen", ":8080", "本地HTTP侦听端口 (e.g., :8080)")
    flag.StringVar(&config.targetURL, "target", "", "反向代理目标 (e.g., https://wishub-x1.ctyun.cn)")
    flag.Func("req-header", "请求头替换规则 (key:pattern:replacement)", func(s string) error {
        config.reqHeader = append(config.reqHeader, s)
        return nil
    })
    flag.Func("req-body", "请求体替换规则 (pattern::replacement)", func(s string) error {
        config.reqBody = append(config.reqBody, s)
        return nil
    })
    flag.Func("resp-header", "应答头替换规则 (key::pattern::replacement)", func(s string) error {
        config.respHeader = append(config.respHeader, s)
        return nil
    })
    flag.Func("resp-body", "应答体替换规则 (pattern::replacement)", func(s string) error {
        config.respBody = append(config.respBody, s)
        return nil
    })

    flag.Parse()

    if config.targetURL == "" {
        log.Fatal("反向代理目标不能为空")
    }

    // 解析目标URL
    target, err := url.Parse(config.targetURL)
    if err != nil {
        log.Fatalf("目标URL格式不对: %v", err)
    }

    // 解析替换规则
    parseRules(config.reqHeader, &reqHeaderRules, true)
    parseRules(config.reqBody, &reqBodyRules, false)
    parseRules(config.respHeader, &respHeaderRules, true)
    parseRules(config.respBody, &respBodyRules, false)
	config.delimiter=unescapeString(config.delimiter)

	if config.debugLevel>=4 { 
		fmt.Printf("DEBUG Chunk delimiter : %s\n",config.delimiter)
		fmt.Printf("DEBUG req-header : %v\n",reqHeaderRules)
		fmt.Printf("DEBUG req-body : %v\n",reqBodyRules)
		fmt.Printf("DEBUG resp-header : %v\n",respHeaderRules)
		fmt.Printf("DEBUG resp-body : %v\n",respBodyRules)
	}

    // 创建反向代理
    proxy := &httputil.ReverseProxy{
        Director: func(req *http.Request) {
			realIP := req.Header.Get("X-Real-IP"); 
			if realIP == "" {
				realIP=req.RemoteAddr
			}
            if config.debugLevel>=3 {
                log.Printf("=== 收到%s请求头 ===\n",realIP)
				fmt.Printf("协议: %s\n", req.Proto)
				fmt.Printf("方法: %s\n", req.Method)
				fmt.Printf("HOST: %s\n", req.Host)
				fmt.Printf("URI: %s\n", req.RequestURI)
                req.Header.Write(os.Stdout)
                fmt.Println("=========")
            }
            req.URL.Scheme = target.Scheme
            req.URL.Host = target.Host
            req.Host = target.Host

            if len(reqHeaderRules) > 0 {
                modifyHeaders(req.Header, reqHeaderRules)
                if config.debugLevel>=3 {
                    fmt.Println("=== 修改后的请求头 ===")
					fmt.Printf("协议: %s\n", req.Proto)
					fmt.Printf("方法: %s\n", req.Method)
					fmt.Printf("HOST: %s\n", req.Host)
					fmt.Printf("URI: %s\n", req.RequestURI)
					req.Header.Write(os.Stdout)
                    fmt.Println("=========")
                }
            } else {
                if config.debugLevel>=3 {
                    fmt.Println("=== 请求头无需修改 ===")

                }
            }

        },
        ModifyResponse: func(resp *http.Response) error {
			req := resp.Request
			realIP := req.Header.Get("X-Real-IP"); 
			if realIP == "" {
				realIP=req.RemoteAddr
			}
            // 如果是流式响应，使用自定义 Body 处理
            if resp.Header.Get("Transfer-Encoding") == "chunked" || strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
                resp.Body = &streamingBody{
                    reader:    resp.Body,
                    rules:     respBodyRules,
                    origBody:  new(bytes.Buffer),
                    totalSize: 0,
					foundDelim: false,
					clientIP:    realIP,
                }
            } else {
                // 非流式响应，整体修改
                body, err := io.ReadAll(resp.Body)
                if err != nil {
                    return err
                }
                resp.Body.Close()

                if config.debugLevel>=3 {
                    log.Println("=== 收到的应答体 ===")
                    fmt.Println(string(body))
                    fmt.Println("=========")
					if config.debugLevel>=4 { fmt.Println(hexDump(body)) }
                }

                modifiedBody := body
                if len(respBodyRules) > 0 {
                    modifiedBody = modifyContent(body, respBodyRules)
                    if config.debugLevel>=3 {
                        fmt.Println("=== 修改后的应答体 ===")
                        fmt.Println(string(modifiedBody))
                        fmt.Println("=========")
						if config.debugLevel>=4 { fmt.Println(hexDump(modifiedBody)) }
                    }
                } else {
                    if config.debugLevel>=3 {
                        fmt.Println("=== 应答体无需修改 ===")

                    }
                }

                resp.Body = io.NopCloser(bytes.NewReader(modifiedBody))
                resp.ContentLength = int64(len(modifiedBody))
            }
			
            if config.debugLevel>=3 {
                log.Println("=== 收到的应答头 ===")
				fmt.Printf("应答状态: %s\n", resp.Status)
				fmt.Printf("协议: %s\n", resp.Proto)
                resp.Header.Write(os.Stdout)
                fmt.Println("=========")
            }
            if len(respHeaderRules) > 0 {
                modifyHeaders(resp.Header, respHeaderRules)
                if config.debugLevel>=3 {
                    fmt.Println("=== 修改后的应答头 ===")
					fmt.Printf("应答状态: %s\n", resp.Status)
					fmt.Printf("协议: %s\n", resp.Proto)
					resp.Header.Write(os.Stdout)
                    fmt.Println("=========")
                }
            } else {
                if config.debugLevel>=3 {
                    fmt.Println("=== 应答头无需修改 ===")

                }
            }
			
            return nil
        },
        Transport: &http.Transport{
            Proxy: http.ProxyFromEnvironment,
        },
        FlushInterval: -1, // 禁用缓冲，确保流式转发
    }

    handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		realIP := r.Header.Get("X-Real-IP"); 
		if realIP == "" {
			realIP=r.RemoteAddr
		}
        body, err := io.ReadAll(r.Body)
        if err != nil {
            http.Error(w, "请求体出错", http.StatusInternalServerError)
            return
        }
        r.Body.Close()
        if config.debugLevel>=3 {
            log.Printf("=== 收到%s的请求体 ===\n",realIP)
            fmt.Println(string(body))
            fmt.Println("=========")
			if config.debugLevel>=4 { fmt.Println(hexDump(body)) }
        }

        modifiedBody := body
        if len(reqBodyRules) > 0 {
            modifiedBody = modifyContent(body, reqBodyRules)
            if config.debugLevel>=3 {
                fmt.Println("=== 修改后的请求体 ===")
                fmt.Println(string(modifiedBody))
                fmt.Println("=========")
 				if config.debugLevel>=4 { fmt.Println(hexDump(modifiedBody))}
           }
        } else {
            if config.debugLevel>=3 {
                fmt.Println("=== 请求体无需修改 ===")
            }
        }
        r.Body = io.NopCloser(bytes.NewReader(modifiedBody))
        r.ContentLength = int64(len(modifiedBody))

        // 如果客户端支持流式响应，确保实时转发
        flusher, ok := w.(http.Flusher)
        if ok {
            w.Header().Set("Transfer-Encoding", "chunked")
			w.Header().Set("Content-Type:", "text/event-stream")
            w.Header().Del("Content-Length") // 移除 Content-Length 以支持 chunked
			//w.Header().Del("Transfer-Encoding")
        }
        proxy.ServeHTTP(&streamingResponseWriter{
            ResponseWriter: w,
            flusher:        flusher,
			clientIP:   realIP,
        }, r)
    })

    log.Printf("HTTP侦听端口 %s, 转发到 %s", config.listenPort, config.targetURL)
    log.Fatal(http.ListenAndServe(config.listenPort, handler))
}

func hexDump(data []byte) string {
	return hex.Dump(data)
}
func unescapeString(s string) string {
	// 替换常见的转义序列
	s = strings.ReplaceAll(s, `\t`, "\t")
	s = strings.ReplaceAll(s, `\n`, "\n")
	s = strings.ReplaceAll(s, `\r`, "\r")
	s = strings.ReplaceAll(s, `\\`, "\\")
	s = strings.ReplaceAll(s, `\"`, "\"")
	return s
}

// streamingBody 自定义 Body，用于流式读取、修改和输出
type streamingBody struct {
    reader    io.ReadCloser
    rules     []RegexReplacement
    origBody  *bytes.Buffer // 记录原始完整内容
    totalSize int           // 记录总大小
	buffer      []byte        // 内部缓冲区
    foundDelim bool          // 是否已找到分隔符
	clientIP string
}

func (s *streamingBody) Read(p []byte) (n int, err error) {
	// 如果已经找到双换行符，直接返回缓冲区的数据
    if s.foundDelim && len(s.buffer) > 0 {
		//s.buffer中为到分隔符位置的chunk部分内容
		n = copy(p, s.buffer)
		s.buffer = s.buffer[n:]
		if config.debugLevel>=3 {
            log.Printf("=== 从远端收到的流式应答内容 (chunk 大小: %d 字节) ===\n", n)
            fmt.Println(string(p[:n]))
			fmt.Println("=========")
			if config.debugLevel>=4 { fmt.Println(hexDump(p[:n])) }
        }
		// 修改 chunk 内容
        if len(s.rules)>0 {
			modifiedChunk := modifyContent(p[:n], s.rules)
			n=copy(p, modifiedChunk)
            if config.debugLevel>=3 {
				fmt.Println("=== 修改后的应答体 ===")
				fmt.Println(string(modifiedChunk))
				fmt.Println("=========")
				if config.debugLevel>=4 { fmt.Println(hexDump(modifiedChunk)) }
			}
			if n!=len(modifiedChunk) {
				log.Printf("奇怪，修改后内容长度与复制缓冲区的长度不一致\n")
			}
		} else {
			if config.debugLevel>=3 {
				fmt.Println("=== 应答体无需修改 ===")

			}
		}

        return n, nil
    }
	
    n, err = s.reader.Read(p)
    if n > 0 {
        s.origBody.Write(p[:n]) // 记录原始内容
        s.totalSize += n

        // 将新数据追加到缓冲区
        s.buffer = append(s.buffer, p[:n]...)

        // 检查缓冲区中是否有分隔符
        if idx := bytes.Index(s.buffer, []byte(config.delimiter)); idx != -1 {
            s.foundDelim = true
            // 截取到双换行符后的位置，s.buffer中为到分隔符位置的chunk部分内容
            s.buffer = s.buffer[:idx+len(config.delimiter)]
        }

        // 如果找到双换行符或者读取结束，处理并输出
        if s.foundDelim || err == io.EOF {
		
			n = copy(p, s.buffer)
			s.buffer = s.buffer[n:]
			if config.debugLevel>=3 {
				log.Printf("=== 从远端收到的流式应答内容 (chunk 大小: %d 字节) ===\n", n)
				fmt.Println(string(p[:n]))
				fmt.Println("=========")
				if config.debugLevel>=4 { fmt.Println(hexDump(p[:n])) }
			}
			// 修改 chunk 内容
			if len(s.rules)>0 {
				modifiedChunk := modifyContent(p[:n], s.rules)
				n=copy(p, modifiedChunk)
				if config.debugLevel>=3 {
					fmt.Println("=== 修改后的应答体 ===")
					fmt.Println(string(modifiedChunk))
					fmt.Println("=========")
					if config.debugLevel>=4 { fmt.Println(hexDump(modifiedChunk)) }
				}
				if n!=len(modifiedChunk) {
					log.Printf("奇怪，修改后内容长度与复制缓冲区的长度不一致\n")
				}
			} else {
				if config.debugLevel>=3 {
					fmt.Println("=== 应答体无需修改 ===")

				}
			}
			return n, nil
		}
    }
	
	// 如果没有找到双换行符且没有错误，返回0,nil让调用者继续读取
    if !s.foundDelim && err == nil {
        return 0, nil
    }

    // 返回缓冲区的数据
    if len(s.buffer) > 0 {
        n = copy(p, s.buffer)
        s.buffer = s.buffer[n:]
        return n, err
    }
    return 0, err
}

func (s *streamingBody) Close() error {
    if config.debugLevel>=1 {
        fmt.Printf("=== 到%s流式应答完成，总大小: %d字节 ===\n\n",s.clientIP, s.totalSize)
    }
    return s.reader.Close()
}

// streamingResponseWriter 自定义 ResponseWriter，用于流式转发
type streamingResponseWriter struct {
    http.ResponseWriter
    flusher http.Flusher
	clientIP string
}

func (w *streamingResponseWriter) Write(b []byte) (int, error) {
    n, err := w.ResponseWriter.Write(b)
    if n > 0 && w.flusher != nil {
        w.flusher.Flush() // 实时推送给客户端
        if config.debugLevel>=2 {
            log.Printf("=== 向客户端%s发送应答 (%d 字节) ===\n", w.clientIP,n)
        }
        if config.debugLevel>=4 {
            fmt.Println(hexDump(b)) 
        }
    }
    return n, err
}

func (w *streamingResponseWriter) WriteHeader(statusCode int) {
    w.ResponseWriter.WriteHeader(statusCode)
}

// 解析替换规则，支持通配符和替换次数
func parseRules(rules []string, target *[]RegexReplacement, isHeader bool) {
    for i, rule := range rules {
        parts := strings.SplitN(rule, "::", 4)
        var pattern, replacement, headerKey string
        var count int = -1

        if isHeader {
            if len(parts) < 3 {
                log.Printf("头部改写规则格式不对: %s (例 key:pattern:replacement)", rule)
                continue
            }
            headerKey = parts[0]
            pattern = parts[1]
            replacement = parts[2]
            if len(parts) == 4 && parts[3] != "" {
				var err error
                count, err = strconv.Atoi(parts[3])
                if err != nil {
                    log.Printf("替换次数字段格式不对 %s: %v", rule, err)
                    continue
                }
            }
        } else {
            if len(parts) < 2 {
                log.Printf("内容改写规则格式不对: %s (例 pattern:replacement)", rule)
                continue
            }
            pattern = parts[0]
            replacement = parts[1]
            if len(parts) == 3 && parts[2] != "" {
				var err error
                count, err = strconv.Atoi(parts[2])
                if err != nil {
                    log.Printf("替换次数字段格式不对 %s: %v", rule, err)
                    continue
                }
            }
        }

        // 将通配符转换为正则表达式
        pattern = regexp.QuoteMeta(pattern)
        pattern = strings.ReplaceAll(pattern, `\*`, ".*")
        pattern = strings.ReplaceAll(pattern, `\?`, "[.\\s]")
        pattern = strings.ReplaceAll(pattern, `\\n`, "\\n")
		replacement=strings.ReplaceAll(replacement, `\n`, "\n")
        if config.debugLevel>=4 { log.Printf("DEBUG 正则匹配 %s，替换 %s\n", pattern, replacement) }
		
 		
        regex, err := regexp.Compile(pattern)
        if err != nil {
            log.Printf("字符串匹配异常 %s: %v", pattern, err)
            continue
        }

        *target = append(*target, RegexReplacement{
			id: i,
            Pattern:     regex,
            Replacement: replacement,
            Count:       count,
            HeaderKey:   headerKey,
        })
    }
}


// 修改headers
func modifyHeaders(headers http.Header, rules []RegexReplacement) {
    for _, rule := range rules {
		if config.debugLevel>=4 { log.Printf("DEBUG 匹配规则: %v", rule)}
        if rule.HeaderKey == "" {
            continue
        }
        values, ok := headers[rule.HeaderKey]
		if config.debugLevel>=4 { log.Printf("DEBUG 修改前的头部信息: %v", values)}
        if !ok {
			if config.debugLevel>=4 { log.Printf("DEBUG 增加头部 %s : %v", rule.HeaderKey, rule.Replacement)}
			headers.Set(rule.HeaderKey,rule.Replacement)
            continue
        }
		if rule.Replacement=="" {
			if config.debugLevel>=4 { log.Printf("DEBUG 删除头部 %s", rule.HeaderKey)}
			headers.Del(rule.HeaderKey)
			continue
		}
        for i, value := range values {
            if rule.Count == -1 {
                values[i] = rule.Pattern.ReplaceAllString(value, rule.Replacement)
            } else {
                remaining := rule.Count
                values[i] = rule.Pattern.ReplaceAllStringFunc(value, func(s string) string {
                    if remaining > 0 {
                        remaining--
                        return rule.Replacement
                    }
                    return s
                })
            }
        }
		if config.debugLevel>=4 { log.Printf("DEBUG 头部信息修改 %s values: %v", rule.HeaderKey, values)}
        headers[rule.HeaderKey] = values
    }
}

// 修改内容
func modifyContent(content []byte, rules []RegexReplacement) []byte {
    result := content
    for _, rule := range rules {
        if rule.Count == -1 {
            result = rule.Pattern.ReplaceAll(result, []byte(rule.Replacement))
        } else {
            remaining := rule.Count
            result = rule.Pattern.ReplaceAllFunc(result, func(b []byte) []byte {
                if remaining > 0 {
                    remaining--
                    return []byte(rule.Replacement)
                }
                return b
            })
        }
    }
    return result
}

func replaceStrBytes(strBytes []byte, oldStr, newStr string) ([]byte, error) {
    str := string(strBytes)
    replacedStr := strings.ReplaceAll(str, oldStr, newStr)
    return []byte(replacedStr), nil
}

func keywdStrBytes(strBytes []byte, kwdStr string) bool {
    str := string(strBytes)
    return strings.Contains(str, kwdStr)
}