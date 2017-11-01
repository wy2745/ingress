#bin/sh
echo "10.32.0.5 foo.bar.com" >> /etc/hosts;
echo "10.32.0.5 bar.baz.com" >> /etc/hosts;

tsung -f /root/.tsung/tsung-test.xml start;
