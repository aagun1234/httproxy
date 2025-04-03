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
	"time"
	"sync/atomic"
	"compress/gzip"
)

type Config struct {
	listenPort string
	targetURL  string
	debugLevel int
	addClientIP bool
	delimiter  string	
	reqHeader  []string
	reqBody	[]string
	respHeader []string
	respBody   []string
	statusURI   string
}

type RegexReplacement struct {
	id		  int
	Pattern	 *regexp.Regexp
	Replacement string
	Count	   int // -1 表示替换所有匹配项
	HeaderKey   string
}

var (
	config		   Config
	reqHeaderRules   []RegexReplacement
	reqBodyRules	 []RegexReplacement
	respHeaderRules  []RegexReplacement
	respBodyRules	[]RegexReplacement
	
	currentConns	 int64
	requestCount	 int64
	startTime		time.Time
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
	flag.BoolVar(&config.addClientIP,"addip", false, "添加XForwardedFor")
	flag.StringVar(&config.delimiter,"delim", "\n\n", "流模式chunk分隔符")
	flag.StringVar(&config.statusURI,"stat", "", "当前状态")
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
 			xff :=req.Header.Get("X-Forwarded-For");
			if xff!="" {
				xff = xff+","+req.RemoteAddr
			} else {
				xff = req.RemoteAddr
			}
			if config.addClientIP {
				req.Header.Set("X-Forwarded-For",xff)
			}
			if config.debugLevel>=3 {
				log.Printf("=== 收到%s请求头 ===\n",realIP)
				fmt.Printf("协议: %s\n", req.Proto)
				fmt.Printf("方法: %s\n", req.Method)
				fmt.Printf("HOST: %s\n", req.Host)
				fmt.Printf("URI: %s\n", req.RequestURI)
				req.Header.Write(os.Stdout)
				fmt.Println("=========\n")
			} else if config.debugLevel>=1 {
				log.Printf("=== 来自%s请求 ===\n",realIP)
			}
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host

			if len(reqHeaderRules) > 0 {
				modifyHeaders(req.Header, reqHeaderRules, realIP)
				if config.debugLevel>=3 {
					fmt.Println("=== 修改后的请求头 ===")
					fmt.Printf("协议: %s\n", req.Proto)
					fmt.Printf("方法: %s\n", req.Method)
					fmt.Printf("HOST: %s\n", req.Host)
					fmt.Printf("URI: %s\n", req.RequestURI)
					req.Header.Write(os.Stdout)
					fmt.Println("=========\n")
				}
			} else {
				if config.debugLevel>=3 {
					fmt.Println("=== 请求头无需修改 ===\n")

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
			if config.debugLevel>=3 {
				log.Println("=== 收到的应答头 ===")
				fmt.Printf("应答状态: %s\n", resp.Status)
				fmt.Printf("协议: %s\n", resp.Proto)
				resp.Header.Write(os.Stdout)
				fmt.Println("=========\n")
			}
			if len(respHeaderRules) > 0 {
				modifyHeaders(resp.Header, respHeaderRules, realIP)
				if config.debugLevel>=3 {
					fmt.Println("=== 修改后的应答头 ===")
					fmt.Printf("应答状态: %s\n", resp.Status)
					fmt.Printf("协议: %s\n", resp.Proto)
					resp.Header.Write(os.Stdout)
					fmt.Println("=========\n")
				}
			} else {
				if config.debugLevel>=3 {
					fmt.Println("=== 应答头无需修改 ===\n")

				}
			}

			if resp.Header.Get("Transfer-Encoding") == "chunked" || strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
				resp.Body = &streamingBody{
					reader:	resp.Body,
					rules:	 respBodyRules,
					origBody:  new(bytes.Buffer),
					totalSize: 0,
					foundDelim: false,
					clientIP:	realIP,
				}
			} else {
                // 非流式响应
				var body []byte
				var err error
				if resp.Header.Get("Content-Encoding") == "gzip" {
					// 解压 Body
					reader, err := gzip.NewReader(resp.Body)
					if err != nil {
						return err
					}
					defer reader.Close()
					body, err = io.ReadAll(reader)
					resp.Header.Del("Content-Encoding") 
					if err != nil {
						return err
					}
				} else {
					body, err = io.ReadAll(resp.Body)
					if err != nil {
						return err
					}
					defer resp.Body.Close()
				}

				// 非流式响应，整体修改

				if config.debugLevel>=3 {
					log.Println("=== 收到的应答体 ===")
					fmt.Println(string(body))
					if config.debugLevel>=4 { fmt.Println(hexDump(body)) }
					fmt.Println("=========\n")
				}

				modifiedBody := body
				if len(respBodyRules) > 0 {
					modifiedBody = modifyContent(body, respBodyRules, realIP)
					if config.debugLevel>=3 {
						fmt.Println("=== 修改后的应答体 ===")
						fmt.Println(string(modifiedBody))
						if config.debugLevel>=4 { fmt.Println(hexDump(modifiedBody)) }
						fmt.Println("=========\n")
					}
				} else {
					if config.debugLevel>=3 {
						fmt.Println("=== 应答体无需修改 ===\n")

					}
				}

				resp.Proto = "HTTP/1.1"
				resp.ProtoMajor = 1
				resp.ProtoMinor = 1
				resp.Body = io.NopCloser(bytes.NewReader(modifiedBody))
				resp.TransferEncoding = nil 
				resp.Header.Del("Transfer-Encoding")
				resp.Header.Set("Content-Length", fmt.Sprint(len(modifiedBody)))
				resp.ContentLength = int64(len(modifiedBody))

			}
						
			return nil
		},
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
		},
		FlushInterval: -1, // 禁用缓冲，确保流式转发
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		
		if config.statusURI!="" {
			if strings.HasPrefix(r.URL.Path, config.statusURI) {
				// 指定资源特殊处理
				respstr:=fmt.Sprintf("Running. CurrentConns: %d, TotalRequests:%d\n",atomic.LoadInt64(&currentConns),atomic.LoadInt64(&requestCount))
				w.Write([]byte(respstr))
				return
			}
		} 
		atomic.AddInt64(&requestCount, 1) // 请求数+1
		atomic.AddInt64(&currentConns, 1) // 连接数+1
		defer atomic.AddInt64(&currentConns, -1)
	
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
			if config.debugLevel>=4 { fmt.Println(hexDump(body)) }
			fmt.Println("=========\n")
		}

		modifiedBody := body
		if len(reqBodyRules) > 0 {
			modifiedBody = modifyContent(body, reqBodyRules, realIP)
			if config.debugLevel>=3 {
				fmt.Println("=== 修改后的请求体 ===")
				fmt.Println(string(modifiedBody))
 				if config.debugLevel>=4 { fmt.Println(hexDump(modifiedBody))}
				fmt.Println("=========\n")
		   }
		} else {
			if config.debugLevel>=3 {
				fmt.Println("=== 请求体无需修改 ===\n")
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
			flusher:		flusher,
			clientIP:   realIP,
	
		}, r)
	})
	currentConns=0
	requestCount=0
	log.Printf("HTTP侦听端口 %s, 转发到 %s", config.listenPort, config.targetURL)
	startTime=time.Now()
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

func varString(s string,clientip string) string {
	if !strings.Contains(s, "$$") {
		return s
	}
	now := time.Now()
	elapsed := time.Since(startTime)
	running := fmt.Sprintf("%d",elapsed.Seconds())
	start := startTime.Format("2006-01-02 13:04:05")
	year := fmt.Sprintf("%02d", now.Year())
	month := fmt.Sprintf("%02d", now.Month()) // 月份补零
	day := fmt.Sprintf("%02d", now.Day())     // 日补零
	hour := fmt.Sprintf("%02d", now.Hour())   // 小时补零
	minute := fmt.Sprintf("%02d", now.Minute())
	second := fmt.Sprintf("%02d", now.Second())
	datestr := now.Format("2006-01-02")
	datetimestr := now.Format("2006-01-02 15:04:05")
	timestr := now.Format("15:04:05")
	
	
	//替换自定义变量
	s = strings.ReplaceAll(s, `$$TIME$$`, timestr)
	s = strings.ReplaceAll(s, `$$DATETIME$$`, datetimestr)
	s = strings.ReplaceAll(s, `$$DATE$$`, datestr)
	s = strings.ReplaceAll(s, `$$YEAR$$`, year)
	s = strings.ReplaceAll(s, `$$MONTH$$`, month)
	s = strings.ReplaceAll(s, `$$DAY$$`, day)
	s = strings.ReplaceAll(s, `$$HOUR$$`, hour)
	s = strings.ReplaceAll(s, `$$MINUTE$$`, minute)
	s = strings.ReplaceAll(s, `$$SECOND$$`, second)
	
	s = strings.ReplaceAll(s, `$$RUNNINGTIME$$`, running)
	s = strings.ReplaceAll(s, `$$STARTTIME$$`, start)
	s = strings.ReplaceAll(s, `$$CURRENTCONNS$$`, fmt.Sprintf("%d",atomic.LoadInt64(&currentConns)))
	s = strings.ReplaceAll(s, `$$REQUESTS$$`, fmt.Sprintf("%d",atomic.LoadInt64(&requestCount)))
	
	s = strings.ReplaceAll(s, `$$ClientIP$$`, clientip)
	return s

}
// streamingBody 自定义 Body，用于流式读取、修改和输出
type streamingBody struct {
	reader	io.ReadCloser
	rules	 []RegexReplacement
	origBody  *bytes.Buffer // 记录原始完整内容
	totalSize int		   // 记录总大小
	buffer	  []byte		// 内部缓冲区
	foundDelim bool		  // 是否已找到分隔符
	clientIP string
}



func (s *streamingBody) Read(p []byte) (n int, err error) {
	posDeli:=0
	s.foundDelim = false
	for {
		n, err = s.reader.Read(p)
		if config.debugLevel>=4 {
			log.Printf("DEBUG 读取%d字节\n", n)
		}
		
		if n > 0 {
			s.origBody.Write(p[:n]) // 记录原始内容
			s.totalSize += n

			// 将新数据追加到缓冲区
			s.buffer = append(s.buffer, p[:n]...)

			// 检查缓冲区中是否有分隔符
			if idx := bytes.Index(s.buffer, []byte(config.delimiter)); idx != -1 {
				s.foundDelim = true
				// 截取到双换行符后的位置，s.buffer中为到分隔符位置的chunk部分内容
				//s.buffer[:=idx+len(config.delimiter)]
				posDeli=idx+len(config.delimiter)
				if config.debugLevel>=4 {
					log.Printf("DEBUG 找到分隔符 %d\n",idx)
					fmt.Println(hexDump(s.buffer))
				}
				break 
			} else {
				posDeli= len(s.buffer)
				if config.debugLevel>=4 {
					log.Printf("DEBUG 还没有找到分隔符\n")
					fmt.Println(hexDump(s.buffer))
				}
			}
		} else {
			if config.debugLevel>=4 {
				log.Printf("DEBUG 读到0字节\n")
				fmt.Println(hexDump(s.buffer))
			}
			if len(s.buffer)>0 {
				break
			} else {
				return 0,err
			}
		}
	}
	
	if err == io.EOF {
		n = copy(p, s.buffer)
		s.buffer = s.buffer[n:]
		posDeli=0
		s.foundDelim=false
		if config.debugLevel>=3 {
			log.Printf("=== 从远端收到的EOF流式应答内容 (chunk 大小: %d 字节) ===\n", n)
			fmt.Println(string(p[:n]))
			if config.debugLevel>=4 { fmt.Println(hexDump(p[:n])) }
			fmt.Println("=========\n")
		}
		// 修改 chunk 内容
		if len(s.rules)>0 {
			modifiedChunk := modifyContent(p[:n], s.rules, s.clientIP)
			n=copy(p, modifiedChunk)
			if config.debugLevel>=3 {
				fmt.Println("=== 修改后的EOF应答体 ===")
				fmt.Println(string(modifiedChunk))
				if config.debugLevel>=4 { fmt.Println(hexDump(modifiedChunk)) }
				fmt.Println("=========\n")
			}
			if n!=len(modifiedChunk) {
				log.Printf("奇怪，修改后内容长度与复制缓冲区的长度不一致\n")
			}
		} else {
			if config.debugLevel>=3 {
				fmt.Println("=== EOF应答体无需修改 ===\n")

			}
		}
		return n, nil
	} else {
		idx := bytes.LastIndex(s.buffer, []byte(config.delimiter))
		if idx >=0 {
			posDeli=idx+len(config.delimiter)
		} else {
			posDeli=len(s.buffer)
		}
		n = copy(p, s.buffer[:posDeli])
		s.buffer = s.buffer[n:]
		if n<posDeli {
			log.Printf("奇怪，复制分隔符前的内容长度与复制缓冲区的长度不一致\n")
		}
		if config.debugLevel>=4 { 
			log.Printf("DEBUG 根据分隔符截断缓冲区\n")
			fmt.Println(hexDump(s.buffer)) 
		}
		posDeli=0
		s.foundDelim=false

		if config.debugLevel>=3 {
			log.Printf("=== 从远端收到的流式应答内容 (chunk 大小: %d 字节) ===\n", n)
			fmt.Println(string(p[:n]))
			if config.debugLevel>=4 { fmt.Println(hexDump(p[:n])) }
			fmt.Println("=========\n")
		}
		// 修改 chunk 内容
		if len(s.rules)>0 {
			modifiedChunk := modifyContent(p[:n], s.rules, s.clientIP)
			n=copy(p, modifiedChunk)
			if config.debugLevel>=3 {
				fmt.Println("=== 修改后的应答体 ===")
				fmt.Println(string(modifiedChunk))
				if config.debugLevel>=4 { fmt.Println(hexDump(modifiedChunk)) }
				fmt.Println("=========\n")
			}
			if n!=len(modifiedChunk) {
				log.Printf("奇怪，修改后内容长度与复制缓冲区的长度不一致\n")
			}
		} else {
			if config.debugLevel>=3 {
				fmt.Println("=== 应答体无需修改 ===\n")

			}
		}
		return n, nil
	}
	
	
	// 如果没有找到分隔符且没有错误，返回0,nil让调用者继续读取
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
		log.Printf("=== 到%s流式应答完成，总大小: %d字节 ===\n\n",s.clientIP, s.totalSize)
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
	if w.ResponseWriter.Header().Get("Content-Length")!="" {
		w.ResponseWriter.Header().Del("Transfer-Encoding")
	}

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
			Pattern:	 regex,
			Replacement: replacement,
			Count:	   count,
			HeaderKey:   headerKey,
		})
	}
}


// 修改headers
func modifyHeaders(headers http.Header, rules []RegexReplacement, cip string) {
	for _, rule := range rules {
		if config.debugLevel>=4 { log.Printf("DEBUG 匹配规则: %v", rule)}
		if rule.HeaderKey == "" {
			continue
		}
		values, ok := headers[rule.HeaderKey]
		if config.debugLevel>=4 { log.Printf("DEBUG 修改前的头部信息: %v", values)}
		if !ok {
			if config.debugLevel>=4 { log.Printf("DEBUG 增加头部 %s : %v", rule.HeaderKey, varString(rule.Replacement,cip))}
			headers.Set(rule.HeaderKey,strings.ReplaceAll(varString(rule.Replacement,cip),"$$ORIGINSTRING$$",""))
			continue
		}
		if rule.Replacement=="" {
			if config.debugLevel>=4 { log.Printf("DEBUG 删除头部 %s", rule.HeaderKey)}
			headers.Del(rule.HeaderKey)
			continue
		}
		for i, value := range values {
			remaining := len(rule.Pattern.FindAllString(value, -1))
			
			if rule.Count != -1 {
				remaining = rule.Count
			}
			values[i] = rule.Pattern.ReplaceAllStringFunc(value, func(s string) string {
				if remaining > 0 {
					remaining--
					return strings.ReplaceAll(varString(rule.Replacement,cip),"$$ORIGINSTRING$$",s)
				}
				return s
			})
		}
		if config.debugLevel>=4 { log.Printf("DEBUG 头部信息修改 %s values: %v", rule.HeaderKey, values)}
		headers[rule.HeaderKey] = values
	}
}

// 修改内容
func modifyContent(content []byte, rules []RegexReplacement, cip string) []byte {
	result := content
	for _, rule := range rules {
		remaining := len(rule.Pattern.FindAll(result, -1))
		if rule.Count != -1 {
			remaining = rule.Count
		}
		result = rule.Pattern.ReplaceAllFunc(result, func(b []byte) []byte {
			if remaining > 0 {
				remaining--
				retstr:=strings.ReplaceAll(varString(rule.Replacement,cip),`$$ORIGINSTRING$$`,string(b))
                return []byte(retstr)
			}
			return b
		})
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