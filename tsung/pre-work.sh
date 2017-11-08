#bin/sh
echo "10.40.0.2 test.java.com" >> /etc/hosts;

tsung -f /root/.tsung/tsung-test.xml start;
