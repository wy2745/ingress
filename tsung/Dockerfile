FROM erlang:20

MAINTAINER Andrea Usuelli <andreausu@gmail.com>

COPY ./tsung-1.7.0.tar.gz /tmp/tsung-1.7.0.tar.gz
#COPY ./pre-work.sh /home/pre-work.sh

RUN apt-get update  \
    && apt-get install -y gnuplot \
    && cd /tmp/ \
    && tar xzf ./tsung-1.7.0.tar.gz \
    && cd tsung-1.7.0 \
    && ./configure \
    && make \
    && make install


#CMD /home/pre-work.sh
#由于这里配合使用的yaml文件会将/root/.tsung挂载到host的目录，这里也使用挂载的目录，应该可以动态执行指令的效果
CMD /root/.tsung/pre-work.sh

EXPOSE 8091


