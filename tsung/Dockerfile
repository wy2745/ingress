FROM erlang:20

MAINTAINER Andrea Usuelli <andreausu@gmail.com>

COPY ./tsung-1.7.0.tar.gz /tmp/tsung-1.7.0.tar.gz
COPY ./pre-work.sh /home/pre-work.sh

RUN apt-get update  \
    && apt-get install -y gnuplot \
    && cd /tmp/ \
    && tar xzf ./tsung-1.7.0.tar.gz \
    && cd tsung-1.7.0 \
    && ./configure \
    && make \
    && make install


CMD /home/pre-work.sh

EXPOSE 8091


